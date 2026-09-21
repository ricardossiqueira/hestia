# Handoff — API local (Connect-RPC) e pendências

> Escrito para: um agente (ou sessão futura) sem nenhum contexto da conversa
> em que isso foi implementado. Leia isto antes de mexer em `internal/api`,
> `internal/deviceprofile`, `justfile` ou no `gateway.yaml` de produção.

## O que é este projeto

`iot-gateway`: gateway Go num Orange Pi Zero 2W que roteia MQTT entre
dispositivos ESP32 locais (via Mosquitto) e, no futuro, uma VPS. Leia
`docs/README.md` primeiro — é o índice de toda a documentação de referência
(`architecture.md`, `mqtt.md`, `configuration.md`, `decisions.md` com as
ADRs, `deployment.md`). Este arquivo (`HANDOFF.md`) não é documentação de
referência do projeto — é um retrato de estado para continuar o trabalho e
deve ser apagado ou reescrito quando as pendências abaixo forem resolvidas.

## O que acabou de ser entregue (concluído e verificado)

Uma API local Connect-RPC (`internal/api`, porta opcional configurável, hoje
usada em `0.0.0.0:8082` na produção) substituiu o antigo `POST /commands`
(`internal/commandapi`, removido). Ela expõe:

- `DeviceService.ListDevices` / `ListDeviceCommands` / `PublishCommand`
- `GatewayService.GetStatus`

`PublishCommand` valida `parameters` contra um schema Protobuf quando o
device declara `profile:` no `gateway.yaml` (`internal/deviceprofile`, um
registry compilado — hoje só `led.v1`, `SetLed{ optional bool on = 1 }`).
Sem `profile:`, cai num fallback opaco (JSON sem validação), igual ao
comportamento antigo. Ver **`docs/api-v1.md`** para o contrato completo e
**`docs/decisions.md`** ADR-009 a ADR-012 para o porquê de cada decisão.

**Como foi construído:** um time de três agentes nesta mesma sessão —
architect (Sonnet, projetou e escreveu os protos + `internal/deviceprofile` +
`internal/api` com testes), implementer (Haiku, aplicou os diffs mecânicos
restantes a partir de um arquivo de tarefas detalhado do architect), validator
(Haiku, provou o comportamento fim a fim com um gateway real + broker MQTT
real + `curl` real, incluindo confirmar que comandos rejeitados **não**
publicam nada no MQTT). Funcionou bem como padrão caso a próxima etapa
(DeviceAdminService, abaixo) tenha porte parecido.

**Commits:** `f216c38` (feat), `d175ed0` (docs), ambos em `origin/main`.

**Estado em produção (Orange Pi, confirmado funcionando):**
`/etc/iot-gateway/gateway.yaml` tem `api:` (porta 8082) e `led-1`/`led-2` com
`profile: led.v1`. `/etc/iot-gateway/environment` tem
`IOT_GATEWAY_API_USERNAME=admin` / `IOT_GATEWAY_API_PASSWORD=admin` (fraco de
propósito — LAN-trusted only, mesma postura já aceita para as credenciais
MQTT existentes). Testado com sucesso:
```bash
curl -u admin:admin -H 'Content-Type: application/json' \
  -d '{"deviceId":"led-1","type":"set_led","parameters":{"on":true}}' \
  http://127.0.0.1:8082/iot.gateway.api.v1.DeviceService/PublishCommand
```

## Atualização — `gateway-web` e a todolist de próximas iterações

Desde a versão original deste handoff, surgiu `gateway-web` (repositório
irmão, `C:\Users\ricar\Dev\gateway-web`), um painel React que consome esta
API — ainda só em fase de spec (`gateway-web/docs/spec.md`, que é a fonte de
verdade sobre lacunas de API do ponto de vista de um consumidor real). A
seção 8 desse spec reorganiza e substitui a antiga "Pendência 2" abaixo com
mais precisão. A partir de agora, **a todolist priorizada é esta**:

1. ~~**CORS**~~ — ✅ feito (`cors_allowed_origins` em `internal/config`,
   middleware em `internal/api`, commits `4ff6722`/`1cf9274`). Ver
   `docs/api-v1.md`'s seção CORS.
2. **Spike de Basic Auth cross-origin** (não implementação — validação
   manual) — confirmar em Chrome/Edge/Firefox/Safari que `fetch` com Basic
   Auth credentials funciona de forma confiável chamando a API a partir de
   `gateway-web` rodando em `localhost:5173`. Se não for confiável, o plano
   B (já no spec) é uma tela que guarda a credencial só em memória e monta
   `Authorization` manualmente — sem contas/sessões/RBAC.
3. **Decisão de composição de porta** (`gateway-web/docs/spec.md` §8.2) —
   hoje `internal/api` (sandboxed, 8082) e `internal/admin` (root, 8081) são
   processos separados, mas o spec quer `DeviceAdminService` na mesma porta
   pública 8082. Dois processos não escutam a mesma porta; precisa de um
   compositor (proxy na frente dos dois, ou o processo admin passa a servir
   8082 e encaminha `DeviceService`/`GatewayService` por loopback/unix
   socket ao processo sandboxed). Decisão de arquitetura a fechar **antes**
   de implementar o item 4.
4. **`DeviceAdminService`** — contrato já rascunhado em
   `gateway-web/docs/spec.md` §8.1: `ProvisionDevice`, `SetDeviceEnabled`,
   `RemoveDevice`. Reaproveita a lógica de `internal/admin/devices.go`
   (`AddDevice`/`RemoveDevice`/`DeviceExists`, edição via `yaml.Node`),
   só muda o transporte. `ProvisionDevice`/`RemoveDevice` precisam de
   rollback automático em falha parcial (YAML + credencial MQTT + restart).
5. **API de observabilidade/fila** (`gateway-web/docs/spec.md` §8.3) —
   contrato ainda não desenhado; precisa primeiro de um modelo de
   persistência (a SQLite atual é outbox de encaminhamento, não histórico
   operacional). Bloqueia as rotas `/queue` e diagnóstico por device do
   `gateway-web`.
6. **Correlação de `command_result`** — hoje `PublishCommand` é
   fire-and-forget puro; não há como saber se o ESP32 executou. Sem prazo
   definido ("Posterior" na matriz do spec).

## Pendência 1 — bug real no `justfile` (✅ corrigido)

**O que aconteceu:** rodar `just install-service` sobrescreveu
`/etc/iot-gateway/gateway.yaml` (que tinha `led-1`, `led-2`, `api:`,
editados ao vivo) com o conteúdo de `config/gateway.yaml` do checkout local
do Orange Pi — um arquivo **gitignored**, esquecido de uma instalação
antiga. Causou perda de registro de dois dispositivos em produção,
recuperado manualmente. Aconteceu de novo (quase) ao pedir para deployar o
CORS, o que motivou o fix.

**Fix aplicado:** `install-service` foi separada em `install-binary`
(binário + unit + `daemon-reload`, seguro em toda atualização, nunca toca
`gateway.yaml`) e `install-config` (só a cópia do `gateway.yaml`, só para a
primeira instalação numa Orange Pi nova). O nome antigo `install-service`
agora falha alto explicando a separação, em vez de silenciosamente
sobrescrever o config ao vivo. `deploy/iot-gateway-update.sh` (o agente de
atualização automática) foi conferido e nunca teve esse problema — só lê
`gateway.yaml` para `validate`/`healthcheck`, nunca escreve nele, e não
chama nenhuma dessas receitas.

## Pendência 2 — Fase 2: `DeviceAdminService` na mesma API

Hoje registrar/remover dispositivo só existe na UI HTML em 8081
(`internal/admin`, processo root separado — ADR-008 em `docs/decisions.md`,
motivo ainda válido: escreve `/etc/mosquitto/*` e `gateway.yaml`, chama
`systemctl`, coisas que o processo sandboxed do `iot-gateway run` nunca deve
fazer). O objetivo de longo prazo (dito explicitamente pelo usuário ao pedir
esta etapa) é finalmente descartar essa página HTML: mover essas mutações
para a mesma API Connect-RPC, servida pelo **processo root** (não pelo
`iot-gateway run` sandboxed) — ver a seção "Fora de escopo (próxima fase)"
em `docs/api-v1.md` e em `docs/implementation-plan.md`.

**Por onde começar:**
- Ler `internal/admin/devices.go` (`AddDevice`, `RemoveDevice`,
  `DeviceExists`, a edição via `yaml.Node` que preserva comentários/
  formatação) e `internal/admin/provision.go`/`restart.go` — a lógica em si
  não muda, só o transporte por cima dela.
- Provavelmente um novo serviço Protobuf (`DeviceAdminService`?) no mesmo
  pacote `iot.gateway.api.v1` ou um pacote próprio, servido por um
  `internal/api`-like server rodando **dentro do processo `iot-gateway-admin`
  atual** (não do `iot-gateway run`), reaproveitando exatamente a mesma lógica
  de `internal/admin/devices.go`.
- Decisão em aberto que precisa ser levada ao usuário: a UI HTML em
  `internal/admin/templates/index.html` é descartada nesta fase, ou ela vira
  um cliente da nova API (mantendo a página, mas ela passa a chamar
  Connect-RPC em vez de manipular `gateway.yaml` diretamente)? O pedido
  original do usuário foi para "finalmente descartar a página HTML", então
  a expectativa provável é substituição, mas confirme antes de remover a UI.

## Notas de ambiente (Windows/Git Bash, relevantes para continuar)

- `buf` (v1.73.0, via `go install github.com/bufbuild/buf/cmd/buf@v1.73.0`)
  não fica no PATH por padrão numa Bash não-interativa nesta máquina.
  Prefixe qualquer comando que use `buf` ou `just proto-*` com:
  ```bash
  export PATH="$PATH:$(cygpath -u "$(go env GOPATH)\bin")"
  ```
  Isso também foi adicionado a `~/.bashrc` do usuário para sessões
  interativas futuras, mas **não** é herdado por chamadas não-interativas de
  ferramenta — sempre prefixe explicitamente.
- `connectrpc.com/connect@v1.21.0` e o plugin `buf.build/connectrpc/go:v1.21.0`
  já foram confirmados funcionando neste ambiente (resolvem via `go get` e
  `buf generate` respectivamente).
- O diretório é um repositório git de verdade (branch `main`, sincronizado
  com `origin/main`) — uma nota anterior nesta mesma sessão dizendo o
  contrário estava errada; não confie nisso sem checar `git status` você
  mesmo primeiro.
