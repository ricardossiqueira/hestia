# Contrato da API local v1

## Objetivo

Este documento descreve a API local exposta pelo processo `iot-gateway
run`: listagem de dispositivos cadastrados, descoberta do schema de
comandos que cada um aceita, publicação de comandos e leitura do status
do gateway. Ela substitui a página HTML em 8081 (`internal/admin`, que
continua existindo só para mutações de cadastro) e o antigo
`POST /commands` em 8082 (`internal/commandapi`, removido).

O protocolo é Connect-RPC (`connectrpc.com/connect`), que serve gRPC,
gRPC-Web e HTTP/JSON na mesma porta a partir do mesmo `.proto`. O arquivo
[api.proto](../api/proto/iot/gateway/api/v1/api.proto) é a fonte de
verdade dos campos e serviços; este texto explica garantias, autenticação
e exemplos de uso e nunca deve divergir dele.

## Limites desta versão

- Escopo: comandos + leitura (dispositivos, schema de comandos, status).
  Mutação de cadastro (registrar/remover device) continua só na UI de
  admin em 8081 — ver `docs/decisions.md` ADR-008.
- Publicação de comando é fire-and-forget: a resposta confirma que o
  broker aceitou a publicação (QoS 1), não que o dispositivo executou o
  comando.
- Só o profile `led.v1` está registrado nesta etapa
  (`internal/deviceprofile`). Um dispositivo sem `profile:` continua
  funcionando, sem validação de schema (fallback opaco).

## Transporte e autenticação

Igual ao antigo `internal/commandapi` e à UI de admin: HTTP Basic Auth,
uma única credencial vinda de variáveis de ambiente
(`IOT_GATEWAY_API_USERNAME` / `IOT_GATEWAY_API_PASSWORD`), sem TLS.
**Uso restrito à LAN confiável — nunca exponha esta porta à internet.**

Diferente de um interceptor Connect, a autenticação é um middleware HTTP
que envolve todo o mux: uma falha de autenticação sempre volta como HTTP
`401` com o cabeçalho `WWW-Authenticate`, que tanto `curl -u` quanto um
navegador entendem, mesmo antes do corpo da requisição ser interpretado
como Connect/gRPC.

Opcional e desligado por padrão — declare `api:` em `gateway.yaml` (ver
`configs/gateway.example.yaml`) para habilitar.

## Serviços e RPCs

| RPC | Serviço | Uso |
| --- | --- | --- |
| `ListDevices` | `DeviceService` | Lista todos os dispositivos cadastrados, habilitados ou não. |
| `ListDeviceCommands` | `DeviceService` | Descreve os comandos que um dispositivo aceita (schema Protobuf). |
| `PublishCommand` | `DeviceService` | Publica um comando, validado por schema quando o dispositivo tem profile. |
| `GetStatus` | `GatewayService` | Espelha `internal/mqtt.Snapshot`: sessão MQTT, contadores, sem payloads. |

## Profile, validação e fallback opaco

Um dispositivo pode declarar `profile: led.v1` em `gateway.yaml`
(`Device.Profile`). Um profile é uma entrada do registry compilado em
`internal/deviceprofile`: para cada `type` de comando, uma mensagem
Protobuf que É o schema dos `parameters` daquele comando.

- Com profile: `PublishCommand` decodifica `parameters` (um
  `google.protobuf.Struct`) como JSON, valida contra a mensagem Protobuf
  do comando (`protojson`, sem campos desconhecidos, tipos e presença
  verificados) e recodifica no formato canônico (`snake_case`, os mesmos
  nomes de campo que o firmware lê) antes de publicar no MQTT. Um campo
  declarado `optional` na mensagem (rastreio de presença do proto3) deve
  estar presente — `{}` é rejeitado para `set_led`, porque `on` é
  `optional`. A resposta traz `schema_validated: true`.
- Sem profile: `parameters` é serializado como JSON e publicado sem
  validação de schema — o mesmo contrato genérico de `docs/mqtt.md`
  (`command_id`, `type`, `parameters` como objeto JSON). A resposta traz
  `schema_validated: false`.

`ListDeviceCommands` devolve, para cada `type` de comando de um profile,
um `CommandDescriptor` com o nome da mensagem Protobuf e o
`google.protobuf.DescriptorProto` completo dela — suficiente, sozinho,
para um cliente montar um formulário, porque toda mensagem de comando do
registry é autocontida (só campos escalares e tipos aninhados nela mesma;
ver o comentário de `internal/deviceprofile`). Um dispositivo sem profile
devolve `schema_validated: false` e uma lista vazia.

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

Publicar um comando (`led-1`, profile `led.v1`):

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{"deviceId":"led-1","type":"set_led","parameters":{"on":true}}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.DeviceService/PublishCommand
```

`{}`, `{"on":"sim"}` ou `{"ligado":true}` para `led-1` voltam com erro e
**nada é publicado no MQTT**.

Ler o status do gateway:

```bash
curl -u <usuario>:<senha> \
  -H 'Content-Type: application/json' \
  -d '{}' \
  http://<orange-pi>:<porta>/iot.gateway.api.v1.GatewayService/GetStatus
```

## Fora de escopo (próxima fase)

Registrar e remover dispositivo continuam só na UI de admin em 8081. Uma
fase seguinte adiciona `DeviceAdminService` a esta mesma API, servido pelo
processo root — o que permitirá finalmente descartar a página HTML (ver
`docs/implementation-plan.md`).
