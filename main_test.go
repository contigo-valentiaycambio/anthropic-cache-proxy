package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json inválido: %v", err)
	}
	return m
}

func lastBlock(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	msgs := m["messages"].([]any)
	content := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	return content[len(content)-1].(map[string]any)
}

func countCC(b []byte) int { return strings.Count(string(b), "cache_control") }

func TestSystemYUltimoMensajeString(t *testing.T) {
	in := `{"model":"claude-sonnet-5-5","system":"SP","messages":[{"role":"user","content":"hola"}]}`
	out, ok := transformBody([]byte(in), true)
	if !ok {
		t.Fatal("no transformó")
	}
	m := decode(t, out)
	sys := m["system"].([]any)[0].(map[string]any)["cache_control"].(map[string]any)
	if sys["ttl"] != "1h" {
		t.Fatalf("system sin ttl 1h: %v", sys)
	}
	b := lastBlock(t, m)
	if b["text"] != "hola" {
		t.Fatalf("texto alterado: %v", b)
	}
	cc := b["cache_control"].(map[string]any)
	if _, has := cc["ttl"]; has {
		t.Fatalf("mensajes debe ir en 5m (sin ttl): %v", cc)
	}
	if countCC(out) != 2 {
		t.Fatalf("esperaba 2 breakpoints, hay %d", countCC(out))
	}
}

func TestUltimoBloqueToolResult(t *testing.T) {
	in := `{"model":"m","system":"SP","messages":[
	 {"role":"user","content":"x"},
	 {"role":"assistant","content":[{"type":"text","text":"a"},{"type":"tool_use","id":"t1","name":"f","input":{}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`
	out, _ := transformBody([]byte(in), true)
	b := lastBlock(t, decode(t, out))
	if b["type"] != "tool_result" || b["cache_control"] == nil {
		t.Fatalf("tool_result sin marcar: %v", b)
	}
	if countCC(out) != 2 {
		t.Fatalf("esperaba 2 breakpoints, hay %d", countCC(out))
	}
}

func TestPingKeepwarmNoMarcaMensajes(t *testing.T) {
	in := `{"model":"m","system":"SP","messages":[{"role":"user","content":"ping"}]}`
	out, _ := transformBody([]byte(in), false)
	if countCC(out) != 1 {
		t.Fatalf("el ping solo debe llevar el breakpoint del system, hay %d", countCC(out))
	}
}

func TestIdempotente(t *testing.T) {
	in := `{"model":"m","system":"SP","messages":[{"role":"user","content":"hola"}]}`
	once, _ := transformBody([]byte(in), true)
	twice, _ := transformBody(once, true)
	if countCC(twice) != 2 {
		t.Fatalf("doble marcado: %d", countCC(twice))
	}
}

func TestDesactivable(t *testing.T) {
	t.Setenv("CACHE_MESSAGES", "false")
	in := `{"model":"m","system":"SP","messages":[{"role":"user","content":"hola"}]}`
	out, _ := transformBody([]byte(in), true)
	if countCC(out) != 1 {
		t.Fatalf("con CACHE_MESSAGES=false solo debe haber 1, hay %d", countCC(out))
	}
}

func TestBloquesNoMarcables(t *testing.T) {
	cases := []string{
		`{"model":"m","system":"SP","messages":[{"role":"user","content":"   "}]}`,
		`{"model":"m","system":"SP","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"s"}]}]}`,
		`{"model":"m","system":"SP","messages":[]}`,
	}
	for _, in := range cases {
		out, _ := transformBody([]byte(in), true)
		if countCC(out) != 1 {
			t.Fatalf("no debía marcar mensajes en %s (hay %d)", in, countCC(out))
		}
	}
}

func TestSinSystemNoMarcaMensajes(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hola"}]}`
	out, ok := transformBody([]byte(in), true)
	if ok || countCC(out) != 0 {
		t.Fatalf("sin system ni tools no se toca nada")
	}
}
