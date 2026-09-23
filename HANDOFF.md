# Handoff — API local (Connect-RPC) e pendências

> Escrito para: um agente (ou sessão futura) sem nenhum contexto da conversa
> em que isso foi implementado. Leia isto antes de mexer em `internal/api`,
> `internal/admin`, `internal/apigateway`, `justfile` ou no `gateway.yaml`
> de produção.

## O que é este projeto

`iot-gateway`: gateway Go num Orange Pi Zero 2W que roteia MQTT entre
dispositivos ESP32 locais (via Mosquitto) e, no futuro, uma VPS.
`gateway-web` (repositório irmão, `C:\Users\ricar\Dev\gateway-web`) é o
painel React que opera esse gateway pela API local. Leia `docs/README.md`
primeiro — é o índice de toda a documentação de referência
(`architecture.md`, `mqtt.md`, `api-v1.md`, `decisions.md` com as ADRs).
Este arquivo (`HANDOFF.md`) não é documentação de referência — é um
retrato de estado para continuar o trabalho.

## Estado atual da API local

Um único Connect-RPC (`connectrpc.com/connect`, gRPC + gRPC-Web + HTTP/JSON
numa porta só) composto por **dois processos** (ADR-013 em
`docs/decisions.md` — ler antes de mexer em qualquer um dos dois):

- **`iot-gateway admin`** (`iot-gateway-admin.service`, root) — única borda
  pública, em `api.address` (produção: `0.0.0.0:8082`). Autentica (HTTP
  Basic, `IOT_GATEWAY_API_USERNAME`/`PASSWORD`) e aplica CORS
  (`cors_allowed_origins`, opcional). Atende `DeviceAdminService`
  diretamente; encaminha `DeviceService`/`GatewayService` por
  `httputil.ReverseProxy` para o processo sandboxed. **Não tem mais UI
  HTML** (ADR-015) — sem `api:` configurado, recusa iniciar.
- **`iot-gateway run`** (`iot-gateway.service`, sandboxed) — só escuta
  `api.internal_address` (loopback, default `127.0.0.1:8083`), sem
  auth/CORS próprios. Responde `DeviceService`/`GatewayService`
  (`internal/api`) reaproveitando a conexão MQTT já viva (ADR-009).

Serviços expostos:

- `DeviceService`: `ListDevices`, `ListDeviceCommands`, `PublishCommand`
  (valida `parameters` contra Protobuf quando o device tem `profile:` —
  `internal/deviceprofile`, só `led.v1` por enquanto; sem profile, fallback
  opaco).
- `GatewayService`: `GetStatus`.
- `DeviceAdminService`: `ProvisionDevice` (a partir de um template
  compilado, só `esp32_led.v1` por enquanto — `internal/admin/
  device_templates.go`), `SetDeviceEnabled`, `RemoveDevice`. Rollback
  automático só em `ProvisionDevice` (ADR-014). A lógica mora em
  `internal/admin` (`registration.go`, `devices.go`, `provision.go`,
  `restart.go`) — `internal/apigateway` só a consome via a interface
  `DeviceAdmin`, satisfeita por `*admin.Server`.

**`internal/admin` não serve mais HTTP nenhum** — a UI HTML em `:8081` foi
removida (ADR-015) depois que `gateway-web` implementou as três operações
de administração e foi validado fim a fim em produção. `*admin.Server` é
só o motor.

Contrato completo, exemplos `curl`, códigos de erro: `docs/api-v1.md`.

## Todolist de próximas iterações

1. ~~CORS~~, ~~spike de auth cross-origin~~, ~~composição de porta~~,
   ~~`DeviceAdminService`~~, ~~aposentar a UI HTML~~ — todos ✅, ver acima
   e `docs/decisions.md` ADR-010 a ADR-015.
2. **API de observabilidade/fila** (`gateway-web/docs/spec.md` §8.3) —
   contrato ainda não desenhado; precisa primeiro de um modelo de
   persistência (a SQLite atual é outbox de encaminhamento, não histórico
   operacional). Bloqueia as rotas `/queue` e diagnóstico por device do
   `gateway-web`.
3. **Correlação de `command_result`** — hoje `PublishCommand` é
   fire-and-forget puro; não há como saber se o ESP32 executou. Sem prazo
   definido.

## Incidentes reais já resolvidos (histórico, não precisa reler para agir)

- **`just install-service` sobrescrevia `gateway.yaml` de produção** com
  um `config/gateway.yaml` local gitignored e desatualizado — corrigido
  separando em `install-binary` (nunca toca `gateway.yaml`) e
  `install-config` (só primeira instalação).
- **Auto-updater não reiniciava `iot-gateway-admin.service`** depois de
  ADR-013 — `iot-gateway.service` ficava no binário novo,
  `iot-gateway-admin.service` no antigo, `:8082` parava de responder.
  Corrigido: `deploy/iot-gateway-update.sh` agora reinicia os dois, e
  reverte os dois juntos se o admin não subir.

## Notas de ambiente (Windows/Git Bash, relevantes para continuar)

- `buf` (v1.73.0, via `go install github.com/bufbuild/buf/cmd/buf@v1.73.0`)
  não fica no PATH por padrão numa Bash não-interativa nesta máquina.
  Prefixe qualquer comando que use `buf` ou `just proto-*` com:
  ```bash
  export PATH="$PATH:$(cygpath -u "$(go env GOPATH)\bin")"
  ```
  Isso também foi adicionado a `~/.bashrc` do usuário para sessões
  interativas futuras, mas **não** é herdado por chamadas não-interativas
  de ferramenta — sempre prefixe explicitamente.
- `connectrpc.com/connect@v1.21.0` e o plugin
  `buf.build/connectrpc/go:v1.21.0` já foram confirmados funcionando neste
  ambiente (resolvem via `go get` e `buf generate` respectivamente).
- O diretório é um repositório git de verdade (branch `main`, sincronizado
  com `origin/main`).
