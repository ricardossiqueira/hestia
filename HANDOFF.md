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

## Pendência 1 — bug real no `justfile` (ainda não corrigido)

**O que aconteceu:** ao aplicar esta mesma entrega em produção, rodar
`just install-service` sobrescreveu `/etc/iot-gateway/gateway.yaml` (que
tinha `led-1`, `led-2`, `api:`, editados ao vivo) com o conteúdo de
`config/gateway.yaml` do checkout local do Orange Pi — um arquivo
**gitignored** (`.gitignore` tem `config/gateway.yaml`), esquecido de uma
instalação antiga, sem nenhum device além de `orangepi-monitor` e
`cyd-monitor`. Causou perda de registro de dois dispositivos em produção.
Recuperado manualmente durante o handoff, mas a causa raiz **continua no
código** e vai se repetir em qualquer atualização futura.

**Causa exata**, `justfile`:
```
config := "config/gateway.yaml"
install-service config=config:
    go build -o bin/iot-gateway ./cmd/gateway
    ...
    sudo install -m 0640 -o root -g iot-gateway {{config}} /etc/iot-gateway/gateway.yaml
    ...
```
Essa receita faz duas coisas que deveriam ser independentes: instalar o
binário (seguro, deveria rodar em toda atualização) e instalar o
`gateway.yaml` (só faz sentido na primeira instalação — depois disso, o
arquivo é gerenciado ao vivo, via UI de admin ou edição manual).

**Fix sugerido** (ainda não implementado, decisão de design em aberto —
confirme com o usuário antes, não é só mecânico):
- Separar em duas receitas: `install-binary` (só o binário + unit file +
  `daemon-reload`) e `install-config` (só a cópia do `gateway.yaml`, para uso
  explícito na primeira instalação). `install-service` pode virar um alias
  de `install-binary` para não quebrar quem já digita esse nome de cor, ou
  ser removida em favor dos dois nomes novos — vale perguntar ao usuário.
- Atualizar `deploy/README.md`: a seção de instalação inicial continua
  usando os dois passos; a seção "CI and automatic updates" e qualquer
  instrução de "atualizar para uma versão nova" devem deixar claro que só
  `install-binary` (ou equivalente) deve rodar depois do primeiro install.
- Verificar se `deploy/iot-gateway-update.sh` (o agente de atualização
  automática via `iot-gateway-update.timer`) tem o mesmo problema — o
  `deploy/README.md` atual diz "The updater never copies a configuration
  file from Git", então ele provavelmente já está correto e o bug é
  exclusivo do fluxo manual via `just install-service`. Confirme lendo
  `deploy/iot-gateway-update.sh` antes de assumir isso.

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
