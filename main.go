// anthropic-cache-proxy
//
// Proxy transparente que se sienta entre n8n y la API de Anthropic.
// Su único trabajo: interceptar las peticiones POST /v1/messages, inyectar
// el marcador `cache_control: { "type": "ephemeral", "ttl": "1h" }` sobre el
// bloque de system prompt (o, si no hay system, sobre la última herramienta)
// y reenviar todo lo demás SIN modificar. Las respuestas —incluido el
// streaming SSE— se transmiten tal cual, sin tocar.
//
// El AI Agent de n8n y todas sus herramientas siguen funcionando igual:
// solo hay que apuntar la "Base URL" de la credencial de Anthropic a este proxy.
//
// La API key NUNCA se almacena aquí: n8n la envía en la cabecera x-api-key y
// el proxy simplemente la reenvía. Por eso este servicio debe permanecer en la
// red interna (no expuesto públicamente).
//
// TTL de caché: por defecto 1 hora (env CACHE_TTL=1h). Para volver al
// comportamiento de 5 minutos sin tocar código: CACHE_TTL=5m y redeploy.
//
// Segundo breakpoint (nov-2026): con CACHE_MESSAGES=true (default) se marca
// también el ÚLTIMO bloque del último mensaje, con TTL de 5 minutos. Así cada
// iteración del agente (tool_use -> tool_result -> respuesta) relee a precio
// de caché el historial, el contexto dinámico y los resultados de herramientas
// que ya envió en la iteración anterior, en vez de pagarlos completos.
// Se apaga con CACHE_MESSAGES=false y redeploy. Ver cacheLastMessage().
//
// Mantenimiento del caché (keepwarm.go): con KEEPWARM_ENABLED=true, el proxy
// reenvía cada ~50 min el mismo prefijo para que el TTL no expire en los huecos
// de la jornada. Ver keepwarm.go.
//
// Solo biblioteca estándar. Sin dependencias externas.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// healthCheck hace un GET a /healthz en el propio proceso y sale 0/1.
// Se usa como HEALTHCHECK de Docker, ya que la imagen distroless no tiene shell
// ni curl: el contenedor invoca su propio binario con la bandera -health.
func healthCheck() {
	port := getenv("PORT", "8080")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "health:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "health: status", resp.StatusCode)
		os.Exit(1)
	}
	os.Exit(0)
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// betaExtendedTTL: header legado del TTL extendido. Ya no es obligatorio
// en la API directa, pero incluirlo es inofensivo y blinda contra cambios
// silenciosos de defaults (como el de marzo 2026).
const betaExtendedTTL = "extended-cache-ttl-2025-04-11"

// ephemeral es el marcador de caché que Anthropic reconoce.
// TTL configurable vía env CACHE_TTL: "1h" (default) o "5m".
func ephemeral() map[string]any {
	cc := map[string]any{"type": "ephemeral"}
	if getenv("CACHE_TTL", "1h") == "1h" {
		cc["ttl"] = "1h"
	}
	return cc
}

// hasCacheControl indica si algún bloque de la lista ya trae cache_control,
// para que la transformación sea idempotente (no marcar dos veces).
func hasCacheControl(blocks []any) bool {
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok {
			if _, exists := m["cache_control"]; exists {
				return true
			}
		}
	}
	return false
}

// ephemeralMessages es el marcador del segundo breakpoint (mensajes).
// Anthropic exige que un TTL largo nunca vaya DESPUÉS de uno corto; el system
// (1h) va antes que los mensajes, así que aquí basta con 5m. Con
// CACHE_MESSAGES_TTL=1h se iguala al del system.
func ephemeralMessages() map[string]any {
	cc := map[string]any{"type": "ephemeral"}
	if getenv("CACHE_MESSAGES_TTL", "5m") == "1h" && getenv("CACHE_TTL", "1h") == "1h" {
		cc["ttl"] = "1h"
	}
	return cc
}

// cacheMessagesEnabled: segundo breakpoint activo salvo CACHE_MESSAGES=false.
func cacheMessagesEnabled() bool {
	return getenv("CACHE_MESSAGES", "true") != "false"
}

// cacheLastMessage marca con cache_control el último bloque del último
// mensaje. Devuelve true si marcó. Reglas:
//   - Si cualquier mensaje ya trae cache_control, no toca nada (idempotente y
//     respeta al cliente si algún día marca él mismo).
//   - Contenido string -> se convierte a [{type:text, text, cache_control}],
//     que es equivalente para la API.
//   - Bloques que la API no deja marcar (thinking, redacted_thinking) o
//     bloques de texto vacíos: no se marca nada.
func cacheLastMessage(data map[string]any) bool {
	msgs, ok := data["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			return false
		}
		if blocks, ok := mm["content"].([]any); ok && hasCacheControl(blocks) {
			return false
		}
	}
	last, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok {
		return false
	}
	switch c := last["content"].(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return false
		}
		last["content"] = []any{
			map[string]any{"type": "text", "text": c, "cache_control": ephemeralMessages()},
		}
		return true
	case []any:
		if len(c) == 0 {
			return false
		}
		b, ok := c[len(c)-1].(map[string]any)
		if !ok {
			return false
		}
		switch b["type"] {
		case "thinking", "redacted_thinking":
			return false
		case "text":
			if t, _ := b["text"].(string); strings.TrimSpace(t) == "" {
				return false
			}
		}
		b["cache_control"] = ephemeralMessages()
		return true
	}
	return false
}

// transformBody inyecta los breakpoints de caché. Devuelve el cuerpo
// modificado y true si lo modificó. Si algo no calza (no es JSON, no hay
// system ni tools), devuelve el cuerpo original intacto y false.
// withMessages=false para los pings del keepwarm: su mensaje "ping" no debe
// crear una entrada de caché propia (el ping la contaría como fallo).
//
// Estrategia: un único breakpoint sobre el system prompt. Como en el orden de
// prefijo de Anthropic las tools van ANTES del system, marcar el system cachea
// tools + system en un solo prefijo estable. El contenido dinámico (fecha,
// contexto del paciente) viaja en el mensaje del usuario, DESPUÉS del
// breakpoint, así que no invalida la caché.
func transformBody(raw []byte, withMessages bool) ([]byte, bool) {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return raw, false // no es JSON: pasar tal cual
	}

	marked := false

	// 1) Preferimos marcar el system prompt.
	if sys, ok := data["system"]; ok {
		switch s := sys.(type) {
		case string:
			if strings.TrimSpace(s) != "" {
				data["system"] = []any{
					map[string]any{
						"type":          "text",
						"text":          s,
						"cache_control": ephemeral(),
					},
				}
				marked = true
			}
		case []any:
			if len(s) > 0 {
				if hasCacheControl(s) {
					marked = true // ya estaba marcado, respetamos
				} else if last, ok := s[len(s)-1].(map[string]any); ok {
					last["cache_control"] = ephemeral()
					marked = true
				}
				data["system"] = s
			}
		}
	}

	// 2) Si no hubo system utilizable, marcamos la última herramienta.
	if !marked {
		if tools, ok := data["tools"].([]any); ok && len(tools) > 0 && !hasCacheControl(tools) {
			if last, ok := tools[len(tools)-1].(map[string]any); ok {
				last["cache_control"] = ephemeral()
				marked = true
			}
		}
	}

	if !marked {
		return raw, false
	}

	// 3) Segundo breakpoint sobre el último mensaje (solo si el primero
	// quedó puesto: sin prefijo cacheado no tiene sentido).
	if withMessages && cacheMessagesEnabled() {
		cacheLastMessage(data)
	}

	out, err := json.Marshal(data)
	if err != nil {
		return raw, false // ante cualquier duda, original intacto
	}
	return out, true
}

// shouldTransform: solo transformamos el endpoint de mensajes, nunca
// count_tokens ni otros endpoints (que pasan transparentes).
func shouldTransform(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages")
}

func main() {
	// Modo healthcheck (usado por Docker HEALTHCHECK en imagen distroless).
	if len(os.Args) > 1 && (os.Args[1] == "-health" || os.Args[1] == "--health") {
		healthCheck()
		return
	}

	port := getenv("PORT", "8080")
	upstreamRaw := getenv("UPSTREAM_URL", "https://api.anthropic.com")

	upstream, err := url.Parse(upstreamRaw)
	if err != nil {
		log.Fatalf("UPSTREAM_URL inválida (%q): %v", upstreamRaw, err)
	}

	// ── keepwarm: mantenimiento del caché (ver keepwarm.go) ──
	// Se apaga con KEEPWARM_ENABLED=false; sin esa variable en "true" el
	// módulo solo observa y nunca envía nada.
	warm := newWarmer(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	warm.Start(ctx)

	// Transport afinado para conexiones largas de LLM: sin timeout global,
	// pero con protecciones razonables de conexión.
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// ResponseHeaderTimeout en 0 = sin límite: el modelo puede tardar en
		// responder en generaciones largas no-stream.
		ResponseHeaderTimeout: 0,
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host
			// Si el upstream tiene un path base (raro), se antepone.
			if upstream.Path != "" && upstream.Path != "/" {
				req.URL.Path = strings.TrimRight(upstream.Path, "/") + req.URL.Path
			}
			// TTL extendido: declarar el beta header de forma idempotente.
			if getenv("CACHE_TTL", "1h") == "1h" {
				if b := req.Header.Get("anthropic-beta"); b == "" {
					req.Header.Set("anthropic-beta", betaExtendedTTL)
				} else if !strings.Contains(b, betaExtendedTTL) {
					req.Header.Set("anthropic-beta", b+","+betaExtendedTTL)
				}
			}
		},
		// FlushInterval -1 = vaciar inmediatamente cada chunk: imprescindible
		// para que el streaming SSE llegue token a token sin buffering.
		FlushInterval: -1,
		Transport:     transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			log.Printf("error de proxy hacia upstream: %v", e)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()

	// Healthcheck para EasyPanel / orquestador.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	// Diagnóstico del pinger: estado, último cache_read, fallos.
	mux.HandleFunc("/keepwarm/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(warm.Status())
	})

	// Catch-all: transforma /v1/messages, deja pasar el resto.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if shouldTransform(r) {
			body, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				log.Printf("no se pudo leer el cuerpo: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			isPing := r.Header.Get(keepwarmHeader) != ""
			newBody, changed := transformBody(body, !isPing)
			// keepwarm: capturar el prefijo EXACTO que sale hacia Anthropic.
			// Los pings del propio módulo se excluyen (traen la cabecera) para
			// que no se cuenten como tráfico real de pacientes.
			if !isPing {
				warm.Observe(newBody, r.Header)
			}
			r.Body = io.NopCloser(bytes.NewReader(newBody))
			r.ContentLength = int64(len(newBody))
			// Evitar Content-Length obsoleto: el Transport lo recalcula.
			r.Header.Del("Content-Length")
			if changed {
				log.Printf("cache_control inyectado, ttl=%s mensajes=%v (%d -> %d bytes)", getenv("CACHE_TTL", "1h"), !isPing && cacheMessagesEnabled(), len(body), len(newBody))
			}
		}
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// Sin WriteTimeout: cortaría respuestas largas/streaming.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("anthropic-cache-proxy escuchando en :%s -> %s (cache ttl=%s, cache mensajes=%v ttl=%s)", port, upstream.String(), getenv("CACHE_TTL", "1h"), cacheMessagesEnabled(), getenv("CACHE_MESSAGES_TTL", "5m"))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("servidor detenido: %v", err)
	}
}
