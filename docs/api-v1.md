# Contrato da API local v1

## Objetivo

Este documento descreve a API local: listagem de dispositivos cadastrados,
descoberta do schema de comandos que cada um aceita, publicação de comandos,
leitura do status do gateway e administração de dispositivos
(`DeviceAdminService`). Ela substitui o antigo `POST /commands` em 8082
(`internal/commandapi`, removido) e a UI HTML de admin em 8081
(`internal/admin`, aposentada — ver ADR-015 em `docs/decisions.md`). É o
único caminho de administração hoje; `gateway-web` é o cliente do dia a
dia.

O protocolo é Connect-RPC (`connectrpc.com/connect`), que serve gRPC,
gRPC-Web e HTTP/JSON na mesma porta a partir do mesmo `.proto`. O arquivo
[api.proto](../api/proto/iot/gateway/api/v1/api.proto) é a fonte de
verdade dos campos e serviços; este texto explica garantias, autenticação
e exemplos de uso e nunca deve divergir dele.

**Dois processos compõem esta API** (`docs/decisions.md` ADR-013) — quem
chama de fora nunca precisa saber disso, é transparente:

```text
Browser/curl (LAN)
      |
      | HTTP Basic Auth + CORS
      v
iot-gateway admin (root, :8082 público)
      |
      +-- DeviceService, GatewayService --> reverse proxy (loopback)
      |                                            |
      |                                            v
      |                              iot-gateway run (sandboxed,
      |                              :internal_address, sem auth própria -
      |                              loopback é o limite de confiança)
      |
      `-- DeviceAdminService (atendido aqui mesmo, sem proxy)
```

`iot-gateway run` continua sendo o único processo com a conexão MQTT viva
(por isso `PublishCommand` mora ali), mas nunca mais é alcançável
diretamente da LAN. `iot-gateway admin` (root) é o único que autentica e
aplica CORS.

## Limites desta versão

- Escopo: comandos, leitura e administração de dispositivos
  (`DeviceAdminService`) — único caminho de administração; a UI HTML de
  admin em 8081 foi aposentada (ADR-015 em `docs/decisions.md`).
- Publicação de comando é fire-and-forget: a resposta confirma que o
  broker aceitou a publicação (QoS 1), não que o dispositivo executou o
  comando.
- Comandos são validados contra o schema declarado na revisão de manifest
  vinculada ao dispositivo (`docs/device-manifests.md`). Um dispositivo sem
  manifest vinculado continua funcionando, sem validação de schema
  (fallback opaco).

## Transporte e autenticação

A autenticação e o CORS vivem **só na borda pública** (`internal/apigateway`,
dentro do processo `iot-gateway admin`): HTTP Basic Auth, uma única
credencial vinda de variáveis de ambiente (`IOT_GATEWAY_API_USERNAME` /
`IOT_GATEWAY_API_PASSWORD`, lidas por esse processo — não pelo `iot-gateway
run`), sem TLS. **Uso restrito à LAN confiável — nunca exponha `api.address`
à internet.**

Diferente de um interceptor Connect, a autenticação é um middleware HTTP
que envolve todo o mux: uma falha de autenticação sempre volta como HTTP
`401` com o cabeçalho `WWW-Authenticate`, que tanto `curl -u` quanto um
navegador entendem, mesmo antes do corpo da requisição ser interpretado
como Connect/gRPC.

O processo interno (`internal/api`, dentro de `iot-gateway run`) não exige
nenhuma credencial própria: `api.internal_address` é obrigatoriamente um
endereço loopback (`iot-gateway validate` recusa qualquer outro), e esse
isolamento do sistema operacional — só processos na própria máquina
alcançam aquela porta — é o único limite de confiança dele. A borda já
autenticou a requisição antes de encaminhá-la; duplicar a credencial no
processo interno seria defesa em profundidade deliberadamente descartada
(ADR-013).

Opcional e desligado por padrão — declare `api:` em `gateway.yaml` (ver
`configs/gateway.example.yaml`) para habilitar. `api.internal_address` tem
um padrão (`127.0.0.1:8083`) e normalmente não precisa ser declarado.

## CORS

Desligado por padrão: sem `cors_allowed_origins` declarado, nenhum cabeçalho
`Access-Control-*` é adicionado e um navegador não consegue chamar esta API
entre origens diferentes — só `curl`/outro servidor, que ignoram CORS.

Para um cliente de browser (ex.: `gateway-web`, que fala Connect HTTP/JSON
diretamente do navegador — ver `gateway-web/docs/spec.md`), declare a
allowlist exata de origens em `gateway.yaml`:

```yaml
api:
  address: 0.0.0.0:8082
  cors_allowed_origins:
    - http://localhost:5173
    - http://127.0.0.1:5173
```

Regras:

- **Nunca `*`** — `iot-gateway validate` rejeita a string literal `"*"` e
  qualquer valor que não seja exatamente `scheme://host[:porta]` (sem
  caminho, query, fragmento ou credenciais na URL). Uma resposta CORS com
  credenciais precisa ecoar uma origem específica e permitida; navegadores
  já recusam `*` combinado com `Access-Control-Allow-Credentials: true`, e
  a validação falha cedo em vez de descobrir isso só no navegador.
- Uma requisição de origem **não** listada não recebe nenhum cabeçalho
  `Access-Control-Allow-Origin` — o servidor processa a requisição
  normalmente (CORS é imposto pelo navegador, não pelo servidor), mas o
  navegador bloqueia a resposta antes de entregá-la ao script.
- O preflight `OPTIONS` é respondido **antes** da autenticação HTTP Basic
  (o preflight nunca carrega o cabeçalho `Authorization` sendo negociado) —
  só a requisição de verdade que segue o preflight precisa de credencial.

## Serviços e RPCs

| RPC | Serviço | Uso |
| --- | --- | --- |
| `ListDevices` | `DeviceService` | Lista todos os dispositivos cadastrados, habilitados ou não. |
| `ListDeviceCommands` | `DeviceService` | Descreve os comandos que um dispositivo aceita (schema Protobuf). |
| `PublishCommand` | `DeviceService` | Publica um comando, validado por schema quando o dispositivo tem manifest vinculado. |
| `GetDeviceTelemetry` | `DeviceService` | Devolve a última mensagem `telemetry` aceita de um device, em cache (sem histórico). |
| `GetStatus` | `GatewayService` | Espelha `internal/mqtt.Snapshot`: sessão MQTT, contadores, sem payloads. |
| `GetQueueSummary` | `GatewayService` | Espelha `internal/outbox.Snapshot`: pendentes agora, bytes, idade do item mais antigo. |
| `GetRecentEvents` | `GatewayService` | Log de atividade em memória: mensagens aceitas/rejeitadas e rotas locais, sem payload. |
| `RegisterExistingDevice` | `DeviceAdminService` | Adota um serviço local com identidade MQTT já existente, sem alterar senha. |
| `ListRoutes` | `DeviceAdminService` | Lista as rotas locais persistidas no SQLite. |
| `CreateRoute` | `DeviceAdminService` | Cria uma rota entre um tópico inbound e um `command` habilitados. |
| `RemoveRoute` | `DeviceAdminService` | Remove uma rota local pelo ID. |
| `ProvisionDeviceByIP` | `DeviceAdminService` | Provisiona um device novo pelo IP a partir de um manifest publicado; a resposta nunca contém senha. |
| `SetDeviceEnabled` | `DeviceAdminService` | Habilita/desabilita um device e aplica a politica sem reiniciar o gateway. |
| `RemoveDevice` | `DeviceAdminService` | Revoga a credencial Mosquitto e remove o device do registry SQLite. |
| `ListInconsistencies` | `DeviceAdminService` | Lista provisionamentos cuja compensação (rollback) também falhou. |
| `ResolveInconsistency` | `DeviceAdminService` | Marca uma inconsistência como resolvida, sem tocar registry ou broker. |

`DeviceAdminService` é atendido **diretamente** pelo processo `iot-gateway
admin` — não passa pelo reverse proxy que `DeviceService`/`GatewayService`
usam, porque provisionamento e revogação de credenciais do Mosquitto ainda são
operações privilegiadas. As mudanças normais de policy — devices e rotas no
SQLite — não escrevem YAML nem chamam `systemctl`.

### Rotas locais

`CreateRoute` recebe `route` com `id`, `source_topic`, `destination_topic`,
`command_type`, `qos` e `retain`. A transformação é sempre
`json_command`: o payload JSON de origem passa a ser `parameters` e o gateway
gera um `command_id` novo. A origem deve ser `telemetry`, `state`, `event` ou
`command_result` de um device habilitado; o destino deve ser `command` de um
device habilitado. A mutação só atualiza a revisão do SQLite: o processo MQTT
já em execução aplica o snapshot, sem restart de `iot-gateway`, Mosquitto ou
dos devices.

Exemplo que envia a telemetria do Orange Pi para o CYD provisionado como
`monitor`:

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"route":{"id":"orangepi-monitor-to-monitor","sourceTopic":"devices/orangepi-monitor/telemetry","destinationTopic":"devices/monitor/command","commandType":"render_system_status","qos":1,"retain":false}}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceAdminService/CreateRoute
```

### Telemetria em cache

`GetDeviceTelemetry` não é uma API de histórico/observabilidade (essa
ainda não foi desenhada — ver HANDOFF.md). `internal/mqtt.Gateway` guarda
só a última mensagem `telemetry` aceita por device, em memória, sem
persistência: um restart do processo esquece tudo, e desabilitar/remover
o device limpa a entrada. `available: false` significa "nada recebido
ainda desde que o processo subiu", não um erro; `device_id` desconhecido
continua sendo `connect.CodeInvalidArgument`, como as demais RPCs de
`DeviceService`. `payload` é um `google.protobuf.Struct` (mesmo motivo do
`parameters` de `PublishCommand` — ver a seção abaixo) com o JSON que o
device publicou; `observed_at` é o `timestamp` que o próprio payload
declarou, não o instante da chamada.

### Resumo da fila (outbox)

`GetQueueSummary` espelha `internal/outbox.Snapshot` 1:1: quantas mensagens
estão pendentes de encaminhamento à VPS agora, quantos bytes de payload elas
ocupam, e o horário de enfileiramento da mais antiga. É diferente dos
contadores `outbox_stored/discarded/failed` que `GetStatus` já reporta -
aqueles são acumulados desde que o processo subiu; este é o estado atual da
fila. Só existe outbox para `telemetry`/`state`/`event` de devices com o
respectivo `forwarding.*_to_vps` habilitado (ver [queue.md](queue.md)) -
sem nenhum device configurado assim, o resumo sempre volta zerado. Não expõe
item, payload ou device por item: paginação/filtro por device e histórico de
7 dias continuam fora de escopo (`gateway-web/docs/spec.md` §8.3) até
existir um modelo de persistência novo - a outbox é uma fila de trabalho
pendente, não um log.

Nenhum fluxo de provisionamento atual (`ProvisionDeviceByIP`/
`RegisterExistingDevice`) liga `forwarding.*_to_vps` - só o
`configs/gateway.example.yaml` fictício tem isso. Na prática, hoje, o
resumo da fila sempre volta zerado; não há ainda um jeito de habilitar
encaminhamento por device.

### Atividade recente

`GetRecentEvents` não é a fila acima - é outra coisa que também não existia
antes: um log de atividade em memória (`internal/mqtt.Gateway`, últimos 200
eventos, mais recente primeiro), gravado no mesmo ponto único
(`handleMessage`) que já processa toda mensagem MQTT aceita. Cobre quatro
`outcome`: `accepted`, `rejected` (com `detail` = motivo), e
`route_published`/`route_failed` para os encaminhamentos locais definidos
em `CreateRoute` (`detail` = ID da rota, mais a causa quando falha; `kind`
fica vazio porque uma rota liga dois devices, não um só). Nunca inclui
payload - mesma disciplina do `SlogLogger` que já loga essas mesmas
mensagens hoje (só que sem deixar consultar pela API). Sem persistência:
um restart do processo esquece tudo, como qualquer contador de
`GetStatus`.

**Filtros e paginação** (obrigatórios para não puxar sempre o buffer
inteiro): `device_id` restringe a um device; `since` exclui eventos mais
antigos que o timestamp dado; `limit` (padrão 50, teto 200) define o
tamanho da página; `before_sequence` pagina pra trás — cada
`ActivityEvent` tem um `sequence` monotônico (também serve de chave
estável de UI), então a próxima página é
`before_sequence = <sequence do último evento da página atual>`.
`has_more` na resposta indica se ainda existem eventos mais antigos além
da página devolvida.

## Manifest, validação e fallback opaco

Um dispositivo provisionado pelo fluxo genérico (`ProvisionDeviceByIP`)
fica vinculado a uma revisão publicada de manifest (`manifest_id` +
`manifest_revision`, `docs/device-manifests.md`). É essa revisão — nunca a
mais recente publicada depois — que descreve os `type` de comando
aceitos e o schema textual dos seus `parameters`, mesmo que o manifest
seja arquivado depois.

- Com manifest vinculado: `PublishCommand` decodifica `parameters` (um
  `google.protobuf.Struct`) como JSON e valida contra o schema declarado
  na revisão vinculada (`boolean`, `string`, `number`, `integer`, campos
  obrigatórios e campos desconhecidos são rejeitados), recodificando no
  formato canônico antes de publicar no MQTT. A resposta traz
  `schema_validated: true`.
- Sem manifest vinculado: `parameters` é serializado como JSON e
  publicado sem validação de schema — o mesmo contrato genérico de
  `docs/mqtt.md` (`command_id`, `type`, `parameters` como objeto JSON). A
  resposta traz `schema_validated: false`.

`ListDeviceCommands` devolve, para cada `type` de comando declarado na
revisão vinculada, um `CommandDescriptor` com o schema textual
(`parameters_json`) — suficiente, sozinho, para um cliente montar um
formulário (é o que `gateway-web` faz). Um dispositivo sem manifest
vinculado devolve `schema_validated: false` e uma lista vazia.

Erros de schema (campo desconhecido, tipo errado, campo obrigatório
ausente, `type` de comando desconhecido, dispositivo desconhecido,
desabilitado ou sem tópico `command`) voltam como
`connect.CodeInvalidArgument` — mesma simplificação do MVP anterior
(`internal/commandapi` devolvia sempre HTTP 400), agora expressa como
código gRPC.

## `parameters` como `Struct`, não `bytes`

Ao contrário de `api/proto/iot/gateway/uplink/v1/uplink.proto` (que usa
`bytes parameters_json` para preservar precisão de número), esta API usa
`google.protobuf.Struct`. Um `bytes` em JSON vira base64, o que quebraria
`curl -d` e qualquer formulário simples — exatamente o que esta API
precisa continuar suportando. O custo é que `Struct` decodifica número
como `double`; é aceitável porque `parameters` é validado e recodificado
contra um schema Protobuf logo em seguida (ver ADR-012 em
`docs/decisions.md`). Um parâmetro inteiro acima de 2^53 deve ser
declarado `string` na mensagem do comando.

## Administração de dispositivos

`DeviceAdminService` roda sobre `internal/admin/registration.go` — a mesma
lógica que a antiga UI HTML de admin em 8081 usava, agora sem nenhuma
superfície HTTP própria (ADR-015).

### Provisionamento por IP e adoção de serviço local

`ProvisionDeviceByIP` é o único caminho para cadastrar um device de rede
novo: consulta o endpoint LAN do device, confere `model`/versão contra um
manifest publicado (`docs/device-manifests.md`) e só então cria a
identidade DynSec e entrega a configuração ao NVS. Um device criado assim
já sai vinculado à revisão de manifest usada no cadastro, então
`PublishCommand`/`ListDeviceCommands` já validam seus comandos declarados
sem nenhum passo extra.

`RegisterExistingDevice` é diferente: usa um template compilado em
`internal/admin` (registry `deviceTemplates`), mas só para *adotar* um
serviço local cuja identidade DynSec já existe — nunca para provisionar
credencial nova. Hoje `orangepi_monitor.v1` é o único template marcado para
adoção; representa o coletor local já instalado no Orange Pi. Use o ID
`orangepi-monitor` para registrar `devices/orangepi-monitor/telemetry` no
SQLite. A operação não provisiona, consulta, expõe ou rotaciona senha
MQTT; ela pressupõe a identidade DynSec que o serviço já usa.

### Operação atômica sem reinício

As alterações de devices e rotas são commits curtos no SQLite. O processo
`iot-gateway run` observa a revisão e troca sua policy em memória; não há
restart de serviço como parte de um sucesso. `ProvisionDeviceByIP` continua
com rollback da credencial DynSec quando o registro no SQLite falha. A remoção
não recria automaticamente uma credencial cuja revogação já tenha sido
solicitada: uma falha parcial retorna erro para investigação do operador.

### Inconsistências de provisionamento

Cada operação de `DeviceAdminService` que mexe em mais de um sistema
(registry SQLite + credencial Mosquitto + entrega ao device) já tenta se
compensar sozinha quando um passo falha depois de outro ter sido aplicado —
ver "Operação atômica sem reinício" acima. `ListInconsistencies`/
`ResolveInconsistency` cobrem o que sobra: quando a **própria compensação**
também falha (ex. o registry aceitou o device, mas revogar a credencial
Mosquitto de limpeza também deu erro). Isso não é um reconciliador
assíncrono com retry automático — não existe no `iot-gateway` hoje (ver
HANDOFF.md). É só um registro durável, gravado em
`registry_inconsistencies` (`internal/registry`) no momento da falha, para
que o operador não dependa de ler a resposta HTTP daquela requisição
específica. `ResolveInconsistency` só marca a entrada como tratada; não
mexe no registry nem no broker — o operador corrige por fora (ex.
`mosquitto_ctrl`) e depois confirma aqui. Lista vazia é o estado saudável.

### Códigos de erro

Ao contrário de `DeviceService` (que colapsa a maioria dos erros em
`connect.CodeInvalidArgument` por serem poucos casos de MVP),
`DeviceAdminService` distingue mais, por envolver mutação e privilégio:

| Código | Quando |
| --- | --- |
| `AlreadyExists` | `ProvisionDeviceByIP` para um `device_id` já cadastrado. |
| `InvalidArgument` | `device_id` inválido, `manifest_id`/`template` desconhecido ou device não compatível. |
| `NotFound` | `SetDeviceEnabled`/`RemoveDevice` para um device inexistente; `ResolveInconsistency` para um `id` desconhecido ou já resolvido. |
| `Internal` | Falha do script de provisionamento, escrita em `gateway.yaml` ou `systemctl` — nada que o cliente resolva mudando a requisição. |

## Exemplos `curl`

Connect aceita JSON simples via `POST` com `Content-Type: application/json`
no caminho `/<pacote>.<Serviço>/<RPC>`.

Listar dispositivos:

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceService/ListDevices
```

Descobrir os comandos de um dispositivo:

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"deviceId":"led-1"}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceService/ListDeviceCommands
```

Publicar um comando (`led-1`, manifest vinculado no cadastro):

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"deviceId":"led-1","type":"set_led","parameters":{"on":true}}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceService/PublishCommand
```

`{}`, `{"on":"sim"}` ou `{"ligado":true}` para `led-1` voltam com erro e
**nada é publicado no MQTT**.

Ler a última telemetria em cache de um device (`available:false` se nada
foi recebido ainda desde que o processo subiu):

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"deviceId":"orangepi-monitor"}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceService/GetDeviceTelemetry
```

Ler o status do gateway:

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.GatewayService/GetStatus
```

Ler o resumo da fila (outbox) - pendentes agora, não os contadores
acumulados de `GetStatus`:

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.GatewayService/GetQueueSummary
```

Ler a atividade recente, filtrada por device (sem `deviceId`/`since`/
`limit`, devolve os 50 mais recentes de qualquer device):

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"deviceId":"orangepi-monitor","limit":20}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.GatewayService/GetRecentEvents
```

Próxima página (`hasMore: true` na resposta anterior — `beforeSequence` é
o `sequence` do último evento devolvido):

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"limit":20,"beforeSequence":"118"}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.GatewayService/GetRecentEvents
```

Cadastrar um device novo pelo IP, a partir de um manifest publicado (a
senha MQTT vai direto para o NVS do device; não aparece nesta resposta):

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"deviceId":"led-3","manifestId":"esp32-c3-led","deviceIp":"192.168.15.43"}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceAdminService/ProvisionDeviceByIP
```

Desabilitar um device (fica cadastrado, só para de aceitar comandos):

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"deviceId":"led-3","enabled":false}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceAdminService/SetDeviceEnabled
```

Remover um device (revoga a credencial Mosquitto e tira do `gateway.yaml`):

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"deviceId":"led-3"}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceAdminService/RemoveDevice
```

Listar inconsistências pendentes (vazio é o estado saudável):

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceAdminService/ListInconsistencies
```

Marcar uma inconsistência como resolvida, depois de corrigir por fora:

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"id":"<id da inconsistência>"}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceAdminService/ResolveInconsistency
```

## Fora de escopo (próxima fase)

A UI HTML de admin em 8081 foi aposentada (ADR-015) depois de
`DeviceAdminService`/`gateway-web` validados fim a fim em produção (spec do
`gateway-web`, Marco 2/4) — `DeviceAdminService` é o único caminho de
administração agora. Próximo: API de observabilidade/fila (contadores por
device, histórico) e correlação de `command_result` (`PublishCommand`
continua fire-and-forget até lá) — ver `docs/implementation-plan.md`.
