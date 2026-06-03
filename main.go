// anthropic-cache-proxy
//
// Proxy transparente que se sienta entre n8n y la API de Anthropic.
// Su único trabajo: interceptar las peticiones POST /v1/messages, inyectar
// el marcador `cache_control: { "type": "ephemeral" }` sobre el bloque de
// system prompt (o, si no hay system, sobre la última herramienta) y reenviar
// todo lo demás SIN modificar. Las respuestas —incluido el streaming SSE—
// se transmiten tal cual, sin tocar.
//
// El AI Agent de n8n y todas sus herramientas siguen funcionando igual:
// solo hay que apuntar la "Base URL" de la credencial de Anthropic a este proxy.
//
// La API key NUNCA se almacena aquí: n8n la envía en la cabecera x-api-key y
// el proxy simplemente la reenvía. Por eso este servicio debe permanecer en la
// red interna (no expuesto públicamente).
//
// Solo biblioteca estándar. Sin dependencias externas.
package main

import (
	"bytes"
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

// ephemeral es el marcador de caché que Anthropic reconoce.
func ephemeral() map[string]any {
	return map[string]any{"type": "ephemeral"}
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

// transformBody inyecta el breakpoint de caché. Devuelve el cuerpo modificado
// y true si lo modificó. Si algo no calza (no es JSON, no hay system ni tools),
// devuelve el cuerpo original intacto y false.
//
// Estrategia: un único breakpoint sobre el system prompt. Como en el orden de
// prefijo de Anthropic las tools van ANTES del system, marcar el system cachea
// tools + system en un solo prefijo estable. El contenido dinámico (fecha,
// contexto del paciente) viaja en el mensaje del usuario, DESPUÉS del
// breakpoint, así que no invalida la caché.
func transformBody(raw []byte) ([]byte, bool) {
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
			newBody, changed := transformBody(body)
			r.Body = io.NopCloser(bytes.NewReader(newBody))
			r.ContentLength = int64(len(newBody))
			// Evitar Content-Length obsoleto: el Transport lo recalcula.
			r.Header.Del("Content-Length")
			if changed {
				log.Printf("cache_control inyectado (%d -> %d bytes)", len(body), len(newBody))
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

	log.Printf("anthropic-cache-proxy escuchando en :%s -> %s", port, upstream.String())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("servidor detenido: %v", err)
	}
}
