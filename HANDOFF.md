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
2. ~~**Spike de Basic Auth cross-origin**~~ — ✅ resolvido por construção,
   não por validação manual: testado na prática (401 real ao ligar o
   `gateway-web`), confirmou que o browser **não** reenvia Basic Auth
   nativamente entre origens com `fetch`. Implementado o plano B que o spec
   já prescrevia: `gateway-web/src/api/auth.ts` guarda a credencial só em
   memória e `gateway.ts` monta `Authorization` manualmente em toda
   chamada. Como não depende mais de nenhum comportamento nativo de
   browser, não há mais inconsistência entre Chrome/Firefox/Safari/Edge
   para validar — todos passam pelo mesmo código explícito.
3. ~~**Decisão de composição de porta**~~ — ✅ feito. `internal/apigateway`
   (novo pacote, dentro do processo `iot-gateway admin`, root) é agora a
   **única** borda pública de `api.address` — autentica, aplica CORS, e
   encaminha `DeviceService`/`GatewayService` via `httputil.ReverseProxy`
   para `internal/api` (dentro de `iot-gateway run`, sandboxed), que passa a
   escutar só `api.internal_address` (loopback, default `127.0.0.1:8083`,
   sem auth/CORS própria — o loopback é o limite de confiança). Ver
   ADR-013 em `docs/decisions.md` e a seção "Transporte e autenticação" de
   `docs/api-v1.md`. Migração em produção: mover
   `IOT_GATEWAY_API_USERNAME`/`PASSWORD` de `/etc/iot-gateway/environment`
   para `/etc/iot-gateway/admin-environment` — `api.address` não muda de
   valor, nenhum `.service` muda.
4. ~~**`DeviceAdminService`**~~ — ✅ feito. `ProvisionDevice`,
   `SetDeviceEnabled`, `RemoveDevice` atendidos diretamente por
   `internal/apigateway` (sem proxy), reaproveitando
   `internal/admin/registration.go` (`RegisterDevice`/`DeregisterDevice`,
   novos) sobre `internal/admin/devices.go` (`AddDeviceFromTemplate`/
   `SetDeviceEnabled`, novos; `AddDevice`/`RemoveDevice` existentes
   inalterados). Templates como registry compilado
   (`internal/admin/device_templates.go`, só `esp32_led.v1` por enquanto).
   Rollback automático só em `ProvisionDevice` (credencial Mosquitto
   desfeita se a escrita em `gateway.yaml` falhar); `RemoveDevice` mantém a
   postura de erro + instrução manual (ADR-014). `handleRemoveDevice` (UI
   HTML) foi refatorado para chamar `DeregisterDevice` — mesma sequência,
   mesmo texto de erro, zero mudança de comportamento (suíte
   `internal/admin` inteira + testes novos de rollback confirmam). **UI
   HTML em 8081 continua no ar** — não foi tocada além dessa refatoração
   interna, permanece como contingência até `gateway-web` validar o fluxo
   completo em produção (spec Marco 2).
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

## Pendência 2 — auto-updater não reiniciava o processo admin (✅ corrigido)

**O que aconteceu:** depois de ADR-013 (composição de porta — item #3),
`iot-gateway-admin.service` virou a única borda pública de `:8082`, mas
`deploy/iot-gateway-update.sh` (o timer que atualiza sozinho a cada 5 min)
só reiniciava `iot-gateway.service`. Um deploy manual reproduziu o mesmo
problema: o binário novo foi instalado e `iot-gateway.service` reiniciado,
mas `iot-gateway-admin.service` continuou rodando o binário antigo em
memória (só sabia servir `:8081`) — `:8082` ficou sem ninguém escutando e
`gateway-web` parou de funcionar. Diagnosticado via `ss -tlnp` (mostrou só
`:8081` e `:8083` escutando, nada em `:8082`) e `git log -1`/timestamp do
binário (confirmando que o arquivo em disco já era o novo, só o processo
admin não tinha sido reiniciado).

**Fix aplicado:** `deploy/iot-gateway-update.sh` agora reinicia
`iot-gateway-admin.service` logo depois que `iot-gateway.service` passa no
healthcheck (mesmo binário para os dois — nenhuma instalação nova
necessária, só o restart). Se o admin falhar ao subir, o script reverte
**os dois** serviços para o binário anterior, nunca só um — evita a
mesma inconsistência remotamente. Uma instalação sem a UI de admin
configurada (`iot-gateway-admin.service` não existe) pula essa parte
inteira sem erro. Ver `deploy/README.md`, seção "CI and automatic
updates".

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
