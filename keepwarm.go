// keepwarm — mantenimiento del caché de prompts.
//
// Con TTL de 1 hora, el prefijo (tools + system) de Rebeca se enfría ~6.7 veces
// al día por los huecos de la jornada, y cada arranque en frío es una escritura
// de caché a 2x el precio de entrada. Este módulo reenvía cada ~50 minutos el
// MISMO prefijo con un mensaje mínimo y max_tokens=1, de modo que la lectura
// reinicie el reloj del TTL y las escrituras bajen a ~1 diaria.
//
// Dos decisiones de diseño que hacen que esto sea seguro:
//
//  1. El prefijo NO se reconstruye: se captura tal cual en Observe(), desde el
//     mismo punto del handler donde el body ya pasó por transformBody(). Son
//     los bytes exactos que van hacia Anthropic.
//
//  2. El ping se dirige al PROPIO proxy (127.0.0.1), no directo a la API. Así
//     atraviesa el mismo Director (header anthropic-beta del TTL extendido) y
//     el mismo transformBody (idempotente: ve el cache_control ya presente y lo
//     respeta). No existen dos rutas que puedan divergir.
//
// Verificación de costos: cada ping inspecciona su propio usage. Si Anthropic
// reporta cache_creation en vez de cache_read, el prefijo no coincidió y el
// ping está costando dinero en vez de ahorrarlo; tras varios fallos seguidos el
// módulo se apaga solo.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// keepwarmHeader marca los requests generados por este módulo, para que
// Observe() no los confunda con tráfico real de pacientes.
const keepwarmHeader = "X-Keepwarm"

type warmConfig struct {
	Enabled     bool
	Interval    time.Duration // mínimo entre pings (< TTL de 1h)
	IdleGrace   time.Duration // no pingear si hubo tráfico real reciente
	Tick        time.Duration // cada cuánto se evalúan las condiciones
	WindowStart int           // hora local de inicio (inclusive)
	WindowEnd   int           // hora local de fin (exclusiva)
	Location    *time.Location
	Upstream    string // por defecto, el propio proxy
	MaxFailures int
}

type warmStatus struct {
	Enabled       bool      `json:"enabled"`
	AutoDisabled  bool      `json:"auto_disabled"`
	HasPrefix     bool      `json:"has_prefix"`
	Model         string    `json:"model"`
	PrefixBytes   int       `json:"prefix_bytes"`
	LastRealAt    time.Time `json:"last_real_at"`
	LastPingAt    time.Time `json:"last_ping_at"`
	PingsSent     int64     `json:"pings_sent"`
	CacheReadLast int64     `json:"cache_read_last"`
	Failures      int       `json:"failures"`
}

type warmer struct {
	cfg    warmConfig
	client *http.Client

	mu         sync.Mutex
	model      string
	tools      json.RawMessage
	system     json.RawMessage
	apiKey     string
	apiVersion string
	lastReal   time.Time
	lastPing   time.Time
	pings      int64
	lastRead   int64
	failures   int
	autoOff    bool
}

// newWarmer construye el pinger leyendo la configuración del entorno.
// port es el puerto en el que escucha este mismo proxy.
func newWarmer(port string) *warmer {
	loc, err := time.LoadLocation("America/Bogota")
	if err != nil {
		loc = time.FixedZone("COT", -5*3600)
	}
	cfg := warmConfig{
		Enabled:     getenv("KEEPWARM_ENABLED", "false") == "true",
		Interval:    envDuration("KEEPWARM_INTERVAL", 50*time.Minute),
		IdleGrace:   envDuration("KEEPWARM_IDLE_GRACE", 45*time.Minute),
		Tick:        envDuration("KEEPWARM_TICK", 5*time.Minute),
		WindowStart: envInt("KEEPWARM_WINDOW_START", 7),
		WindowEnd:   envInt("KEEPWARM_WINDOW_END", 21),
		Location:    loc,
		Upstream:    getenv("KEEPWARM_UPSTREAM", "http://127.0.0.1:"+port+"/v1/messages"),
		MaxFailures: envInt("KEEPWARM_MAX_FAILURES", 3),
	}
	return &warmer{
		cfg:    cfg,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(getenv(key, "")); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(getenv(key, "")); err == nil && d > 0 {
		return d
	}
	return def
}

// prefijo mínimo que interesa del body saliente.
type warmPrefix struct {
	Model  string          `json:"model"`
	Tools  json.RawMessage `json:"tools"`
	System json.RawMessage `json:"system"`
}

// Observe recibe el body EXACTO que el handler está a punto de reenviar
// (post-transformBody) y guarda el prefijo. Nunca devuelve error ni modifica
// nada: este módulo jamás debe poder romper la petición de un paciente.
func (w *warmer) Observe(body []byte, h http.Header) {
	if w == nil {
		return
	}
	var p warmPrefix
	if err := json.Unmarshal(body, &p); err != nil {
		return
	}
	if p.Model == "" || len(p.System) == 0 {
		return
	}
	// Sin cache_control no hay nada que refrescar.
	if !bytes.Contains(p.System, []byte("cache_control")) &&
		!bytes.Contains(p.Tools, []byte("cache_control")) {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.model = p.Model
	w.tools = append(json.RawMessage(nil), p.Tools...)
	w.system = append(json.RawMessage(nil), p.System...)
	if h != nil {
		if v := h.Get("x-api-key"); v != "" {
			w.apiKey = v
		}
		if v := h.Get("anthropic-version"); v != "" {
			w.apiVersion = v
		}
	}
	w.lastReal = time.Now()
	// Prefijo nuevo (p. ej. tras desplegar un prompt v20): si el módulo se
	// había auto-apagado, se le da otra oportunidad.
	if w.autoOff {
		w.autoOff = false
		w.failures = 0
		log.Printf("[keepwarm] prefijo actualizado; pinger reactivado")
	}
}

// Start lanza el bucle en una goroutine; termina al cancelar ctx.
func (w *warmer) Start(ctx context.Context) {
	if w == nil {
		return
	}
	log.Printf("[keepwarm] enabled=%v intervalo=%s ventana=%02d:00-%02d:00 upstream=%s",
		w.cfg.Enabled, w.cfg.Interval, w.cfg.WindowStart, w.cfg.WindowEnd, w.cfg.Upstream)
	go func() {
		t := time.NewTicker(w.cfg.Tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := w.maybePing(ctx); err != nil {
					log.Printf("[keepwarm] ping fallido: %v", err)
				}
			}
		}
	}()
}

// shouldPing se llama con el mutex tomado.
func (w *warmer) shouldPing(now time.Time) bool {
	if !w.cfg.Enabled || w.autoOff {
		return false
	}
	if len(w.system) == 0 || w.apiKey == "" {
		return false // aún no ha pasado tráfico real
	}
	h := now.In(w.cfg.Location).Hour()
	if h < w.cfg.WindowStart || h >= w.cfg.WindowEnd {
		return false
	}
	if now.Sub(w.lastReal) < w.cfg.IdleGrace {
		return false // el caché ya está caliente por tráfico real
	}
	if now.Sub(w.lastPing) < w.cfg.Interval {
		return false
	}
	return true
}

func (w *warmer) maybePing(ctx context.Context) error {
	now := time.Now()

	w.mu.Lock()
	if !w.shouldPing(now) {
		w.mu.Unlock()
		return nil
	}
	model, tools, system := w.model, w.tools, w.system
	key, ver := w.apiKey, w.apiVersion
	w.lastPing = now
	w.pings++
	n := w.pings
	w.mu.Unlock()

	modelJSON, err := json.Marshal(model)
	if err != nil {
		return err
	}
	payload := map[string]json.RawMessage{
		"model":      json.RawMessage(modelJSON),
		"system":     system,
		"messages":   json.RawMessage(`[{"role":"user","content":"ping"}]`),
		"max_tokens": json.RawMessage(`1`),
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.Upstream, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", key)
	if ver == "" {
		ver = "2023-06-01"
	}
	req.Header.Set("anthropic-version", ver)
	req.Header.Set(keepwarmHeader, "1")

	resp, err := w.client.Do(req)
	if err != nil {
		return err // error de red: no cuenta como prefijo desalineado
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return errors.New("upstream " + resp.Status + ": " + snippet(raw))
	}

	var parsed struct {
		Usage struct {
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			Input         int64 `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastRead = parsed.Usage.CacheRead

	if parsed.Usage.CacheCreation > 0 {
		w.failures++
		log.Printf("[keepwarm] ALERTA: el ping ESCRIBIO cache (%d tokens) en vez de leerlo. "+
			"El prefijo no coincide con el del trafico real. Fallo %d/%d.",
			parsed.Usage.CacheCreation, w.failures, w.cfg.MaxFailures)
		if w.failures >= w.cfg.MaxFailures {
			w.autoOff = true
			log.Printf("[keepwarm] APAGADO AUTOMATICO tras %d fallos seguidos. "+
				"No se enviaran mas pings hasta que cambie el prefijo o se reinicie.", w.failures)
		}
		return nil
	}

	w.failures = 0
	log.Printf("[keepwarm] ok: cache_read=%d input=%d (ping #%d)",
		parsed.Usage.CacheRead, parsed.Usage.Input, n)
	return nil
}

// Status devuelve la foto del estado interno para el endpoint de diagnóstico.
func (w *warmer) Status() warmStatus {
	if w == nil {
		return warmStatus{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return warmStatus{
		Enabled:       w.cfg.Enabled,
		AutoDisabled:  w.autoOff,
		HasPrefix:     len(w.system) > 0,
		Model:         w.model,
		PrefixBytes:   len(w.system) + len(w.tools),
		LastRealAt:    w.lastReal,
		LastPingAt:    w.lastPing,
		PingsSent:     w.pings,
		CacheReadLast: w.lastRead,
		Failures:      w.failures,
	}
}

// snippet recorta el cuerpo de una respuesta de error. Nunca se loguea el
// request, que lleva la API key en cabecera.
func snippet(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
