# anthropic-cache-proxy

Proxy transparente que habilita el **prompt caching de Anthropic** para el AI Agent de n8n, sin tocar el workflow ni reescribir el agente.

Se sienta entre n8n y `api.anthropic.com`. Intercepta cada `POST /v1/messages`, inyecta `cache_control: { "type": "ephemeral" }` sobre el system prompt (que en tu caso cachea también las definiciones de herramientas, porque van antes en el prefijo) y reenvía la respuesta —incluido el streaming— sin modificarla. n8n ni se entera.

Escrito en Go con biblioteca estándar únicamente: binario estático, imagen final mínima (distroless), consumo de memoria de unos pocos MB y muy estable en conexiones largas.

---

## Por qué resuelve tu caso

- Tu system prompt (~37.700 tokens) es un prefijo **estable**: lo dinámico (fecha, contexto del paciente) viaja en el mensaje del usuario, después del breakpoint, así que no invalida la caché.
- Supera de sobra el mínimo de Sonnet 4.6 (2.048 tokens) para que la caché se active.
- Lectura en caché = 0,1x del precio de entrada (≈90% menos en la parte repetida). Escritura inicial = 1,25x una sola vez por ventana de 5 minutos.

---

## 1. Desplegar en EasyPanel

Tienes dos caminos. El más simple en EasyPanel es por repositorio Git o por Dockerfile.

1. Sube esta carpeta (`main.go`, `go.mod`, `Dockerfile`) a un repositorio Git, o créala como servicio tipo **Dockerfile** en tu proyecto de EasyPanel.
2. En EasyPanel: **+ Service → App**.
   - Source: tu repo (o "Dockerfile" si subes los archivos).
   - Build: detecta el `Dockerfile` automáticamente.
3. En la configuración del servicio:
   - **Port interno**: `8080`.
   - **NO publiques un dominio público** ni expongas el puerto al exterior. Este servicio debe vivir solo en la red interna del proyecto, porque recibe peticiones que llevan tu API key en la cabecera. Déjalo accesible únicamente para tu contenedor de n8n.
   - Variables de entorno (opcionales, ya tienen default):
     - `PORT=8080`
     - `UPSTREAM_URL=https://api.anthropic.com`
4. Deploy. El healthcheck integrado (`/proxy -health` contra `/healthz`) le indica a EasyPanel cuándo está sano.

Anota el **hostname interno** que EasyPanel asigna al servicio. En proyectos de EasyPanel suele ser el nombre del servicio (por ejemplo `anthropic-cache-proxy`), accesible desde n8n como:

```
http://anthropic-cache-proxy:8080
```

(El nombre exacto lo ves en la pestaña del servicio; EasyPanel enruta por el nombre dentro de la misma red del proyecto.)

---

## 2. Apuntar n8n al proxy

Esto es lo único que cambias en n8n, y es a nivel de **credencial**, no de nodo:

1. n8n → **Credentials** → abre tu credencial de Anthropic (la que usa `Anthropic Chat Model`).
2. En el campo **Base URL**, reemplaza `https://api.anthropic.com` por la URL interna del proxy:

   ```
   http://anthropic-cache-proxy:8080
   ```

3. Guarda. No cambies nada más: la API key sigue donde estaba, el workflow sigue igual.

Si tu credencial no muestra el campo "Base URL", confirma que estás en una versión reciente de n8n (las actuales sí lo incluyen).

---

## 3. Verificar que la caché funciona

Manda 2–3 mensajes al bot con menos de 5 minutos entre ellos (para caer dentro de la ventana de caché) y revisa:

- **Logs del proxy** (en EasyPanel): verás líneas `cache_control inyectado (N -> M bytes)` por cada petición a `/v1/messages`. Eso confirma que la inyección ocurre.
- **Anthropic Console → Usage**: el primer mensaje registrará `cache_creation_input_tokens` altos y `cache_read_input_tokens` en 0. A partir del segundo (dentro de la ventana de 5 min) verás `cache_read_input_tokens` altos y la entrada normal casi en cero. Ese es el ahorro.

Regla práctica: si tras una semana el ratio de lectura en caché está por debajo del 20%, casi siempre es porque algo dinámico se está colando en el prefijo estable; en tu arquitectura no debería pasar, porque lo dinámico va en el mensaje del usuario.

---

## Caché de mensajes (segundo breakpoint)

Además del system prompt (TTL 1 h), el proxy marca el **último bloque del último mensaje** con `cache_control` de 5 minutos. En cada turno el agente de n8n llama varias veces a la API (respuesta → herramienta → resultado → respuesta final); con este marcador, la segunda y siguientes llamadas releen a precio de caché el historial, el contexto dinámico y los resultados de herramientas que ya enviaron, en vez de pagarlos completos.

| Variable | Default | Efecto |
|---|---|---|
| `CACHE_MESSAGES` | `true` | `false` lo apaga (solo queda el breakpoint del system) |
| `CACHE_MESSAGES_TTL` | `5m` | `1h` solo si `CACHE_TTL=1h`; la API no admite un TTL largo después de uno corto |

Los pings del keepwarm no llevan este segundo marcador. En los logs, cada petición muestra `mensajes=true`. En la Console, el efecto se ve como menos *Input* y algo de *Prompt caching write (5m)* (1,25× el precio de input, solo sobre lo nuevo de cada llamada).

## Notas importantes

- **Seguridad**: el proxy reenvía la API key que envía n8n; no la guarda. Manténlo en la red interna, sin dominio público. Si algún día lo expones, ponle al menos una cabecera/secreto de acceso.
- **TTL de 5 minutos**: la caché expira a los 5 min de inactividad. Conversaciones activas se benefician; un paciente que escribe una vez al día paga la escritura cada vez (sigue conviniendo si hay varios mensajes seguidos).
- **Streaming**: el proxy usa flush inmediato, así que el agente recibe los tokens igual que en directo.
- **count_tokens y otros endpoints**: pasan sin transformar.
- **Idempotente**: si algún día el cuerpo ya trae `cache_control`, el proxy lo respeta y no duplica el marcador.

---

## Probar localmente (opcional)

```bash
docker compose up --build
# en otra terminal:
curl -s http://localhost:8080/healthz   # -> ok
```

Para una prueba real apuntando al upstream, manda un POST a `http://localhost:8080/v1/messages` con tu `x-api-key` y `anthropic-version`, igual que lo harías contra Anthropic.

---

## Archivos

- `main.go` — el proxy (biblioteca estándar, sin dependencias).
- `go.mod` — módulo Go.
- `Dockerfile` — build multi-stage → imagen distroless con healthcheck.
- `docker-compose.yml` — para pruebas locales.
- `.dockerignore`
