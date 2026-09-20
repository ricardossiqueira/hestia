# Contrato de uplink Gateway--VPS v1

## Objetivo

Este documento define o contrato futuro entre um gateway e a VPS. O uplink
envia dados duraveis da outbox e recebe comandos por *polling*. Ele nao expoe
o broker MQTT, o endpoint de diagnostico nem dispositivos da LAN.

O protocolo e gRPC com Protobuf, transportado em HTTP/2 sobre WireGuard. O
arquivo [uplink.proto](../api/proto/iot/gateway/uplink/v1/uplink.proto) e a
fonte de verdade dos campos e servicos. Este texto explica garantias,
seguranca e operacao; nunca deve divergir do `.proto`.

O repositorio usa Buf 1.73.0 e plugins remotos versionados para gerar os stubs
Go versionados em `api/gen/go`. Use `just proto-lint` para validar o schema e
`just proto-generate` para atualizar os arquivos gerados.

Para desenvolvimento local, instale a mesma versao com
`go install github.com/bufbuild/buf/cmd/buf@v1.73.0` e inclua o diretorio
`$(go env GOPATH)/bin` no `PATH`. O CI instala a versao fixada e falha se a
geracao alterar os stubs versionados.

O gateway continua agnostico: encaminha envelopes genericos e JSON opaco, sem
conhecer CYD, ESP32 especificos ou campos de dominio do payload. O requisito
central e **offline-first**: uma VPS indisponivel nunca pode bloquear MQTT
local, rotas locais nem o armazenamento na outbox.

## Limites desta versao

- Transporte: gRPC/Protobuf sobre HTTP/2, WireGuard e mTLS.
- Entrega gateway -> VPS: pelo menos uma vez no transporte e uma vez logica na
  VPS por deduplicacao.
- Entrega VPS -> gateway: pelo menos uma vez. Publicacoes MQTT repetidas ainda
  sao possiveis; `command_id` e a chave de idempotencia do comando.
- A VPS nunca abre conexao para o Orange Pi. Todo trafego e iniciado pelo
  gateway, inclusive a busca de comandos.

Nao fazem parte da v1: bridge MQTT entre redes, streaming bidirecional,
WebSocket, regras locais, descoberta automatica, atualizacao de firmware e
garantia de execucao fisica "exactly once" no dispositivo.

## Transporte, rede e identidade

A VPS escuta somente no IP da interface WireGuard, por exemplo
`10.77.0.1:8443`; nao deve existir listener publico equivalente. O firewall
aceita a porta apenas pela interface WireGuard.

Mesmo dentro do tunel, gRPC usa TLS 1.3 com autenticacao mutua (mTLS):

- cada gateway recebe certificado de cliente proprio;
- o SAN do certificado identifica o gateway, por exemplo
  `urn:iot-gateway:orangepi-lab-01`;
- `gateway_id` de cada request deve corresponder ao SAN e a um gateway
  provisionado na VPS;
- o gateway valida a CA e o nome/IP esperado da VPS;
- WireGuard restringe a rede; mTLS autentica e autoriza a aplicacao.

Senhas MQTT nunca transitam pelo uplink. Chave privada, certificado de cliente
e CA ficam fora do Git, com permissoes minimas. A chave privada pertence a
`root` e so e legivel pelo usuario/grupo do servico quando isso for necessario
para estabelecer TLS.

Configuracao prevista (ainda nao implementada):

```yaml
uplink:
  enabled: true
  endpoint: 10.77.0.1:8443
  ca_file: /etc/iot-gateway/uplink-ca.pem
  client_cert_file: /etc/iot-gateway/uplink-client.pem
  client_key_file: /etc/iot-gateway/uplink-client-key.pem
  max_batch_messages: 100
  max_batch_bytes: 262144
  request_timeout: 15s
  command_poll_wait: 25s
```

## Convencoes Protobuf

- O pacote e `iot.gateway.uplink.v1`; uma mudanca incompativel cria `v2`.
- IDs novos usam UUID v4 em campos `string`.
- Horarios usam `google.protobuf.Timestamp` em UTC.
- Campos `bytes` terminados em `_json` contem UTF-8 que representa exatamente
  um objeto JSON. Isso preserva o payload de dominio sem usar
  `google.protobuf.Struct`, que pode alterar a precisao de numeros JSON.
- O gateway valida JSON antes de enfileirar e a VPS valida os limites antes de
  persistir. Nenhum lado interpreta seus campos de dominio.
- O tamanho total serializado de um request e no maximo 256 KiB, e cada campo
  `payload_json` ou `result_json` tem no maximo 64 KiB.
- O servidor pode incluir `x-request-id` nos metadados de resposta. Gateway e
  VPS podem registra-lo, mas nunca registram payloads ou segredos.

## Servico

`UplinkService` possui tres RPCs unarios:

| RPC | Direcao | Uso |
| --- | --- | --- |
| `PushBatch` | gateway -> VPS | Confirma lote ordenado da outbox. |
| `PullCommands` | gateway -> VPS | Long polling de uma janela de comandos. |
| `ReportCommandStatus` | gateway -> VPS | Persiste transicao ou resultado. |

RPCs unarios simplificam retomada apos reboot e mantem a outbox como fonte de
verdade. Um stream pode ser avaliado em versao futura, sem substituir as
garantias de ACK duravel.

## Envio da outbox

O gateway chama `PushBatch` com `PushBatchRequest`. Cada `OutboundMessage`
contem `message_id`, `sequence`, `device_id`, `kind`, `topic`, `enqueued_at` e
`payload_json`.

Regras:

- `kind` e somente `TELEMETRY`, `STATE` ou `EVENT`.
- `payload_json` e o objeto JSON validado recebido no MQTT, sem interpretacao
  de seus campos de dominio.
- `sequence` representa a ordem local da outbox. Ajuda observabilidade, mas
  nao e identidade e nao estabelece uma ordem global.
- O gateway monta lotes em ordem crescente de `sequence`, com ate 100 itens e
  tamanho serializado de ate 256 KiB.
- A VPS persiste o lote em uma transacao: ou confirma todos os itens, ou nao
  confirma nenhum.

A resposta explicita `acknowledged_message_ids`. O gateway remove somente
itens presentes nessa lista. A VPS impoe unicidade em
`(gateway_id, message_id)`: uma repeticao identica e confirmada de novo; a
mesma chave com conteudo diferente retorna `ALREADY_EXISTS`.

Se a resposta se perder apos o commit da VPS, o gateway reenviara o lote e a
deduplicacao tornara a operacao segura. Portanto ha entrega pelo menos uma vez
no transporte e uma vez logica na VPS.

## Retentativa e falhas

O consumidor SQLite ja fornece leitura por `sequence`, lease com expiracao,
contador de tentativa, confirmacao explicita por `message_id` e reagendamento
de falhas. O cliente gRPC futuro decide quando chama essas operacoes e calcula
o backoff.

Politica padrao:

- timeout de conexao de 5 s e deadline RPC de 15 s;
- primeira retentativa com atraso aleatorio entre 0 e 1 s;
- backoff exponencial com jitter completo, limitado a 5 min;
- se houver `retry-after-ms` nos metadados, respeita-lo ate 30 min;
- repetir `UNAVAILABLE`, `DEADLINE_EXCEEDED`, `RESOURCE_EXHAUSTED` e erros de
  rede/transporte;
- para `INVALID_ARGUMENT`, `UNAUTHENTICATED`, `PERMISSION_DENIED`,
  `ALREADY_EXISTS`, `FAILED_PRECONDITION` ou `OUT_OF_RANGE`, conservar o dado,
  registrar somente metadados operacionais e aplicar cooldown.

Antes da ativacao, o gateway deve impor o limite de 64 KiB a qualquer mensagem
encaminhavel. Uma mensagem local valida, mas impossivel de transmitir, nao pode
bloquear indefinidamente a cabeca da fila.

## Downlink de comandos

O gateway chama `PullCommands`. `wait_seconds` permite long polling entre 0 e
25 segundos e `limit` fica entre 1 e 20. A VPS devolve imediatamente se houver
comando; do contrario, ao terminar a espera.

A resposta tem `delivery_id`, `lease_expires_at`, `has_more` e comandos. Nao
ha cursor ou offset: a VPS entrega uma janela com lease e devolve comandos nao
confirmados quando o lease expira.

Antes de publicar no MQTT, o gateway deve validar identificadores, datas,
`parameters_json` como objeto JSON e `type` nao vazio; confirmar que o
dispositivo esta declarado, habilitado e tem `commands_from_vps: true`; e
exigir topico `command`. A VPS limita validade a 24 h e o gateway rejeita
comando expirado.

O gateway grava o comando e cada transicao em um ledger SQLite **antes** da
publicacao MQTT. Ele deduplica por `command_id` e publica com QoS 1,
`retain=false`. Assim, nova entrega do mesmo comando nao gera segunda
publicacao. O dispositivo tambem deve tratar `command_id` como chave de
idempotencia para quedas entre publicar e registrar resultado.

## Status e resultado de comandos

`ReportCommandStatus` persiste transicoes idempotentes. Os valores permitidos
sao `RECEIVED`, `REJECTED`, `PUBLISHED` e `RESULT`.

`PUBLISHED` quer dizer que o broker aceitou QoS 1, nao que o dispositivo
executou o comando. Para `REJECTED`, usa-se codigo generico como
`device_not_authorized`, `invalid_command`, `expired` ou
`mqtt_publish_failed`, sem detalhes sensiveis.

`RESULT` inclui o JSON MQTT original em `result_json` e exige `command_id` UUID
no resultado. Resultados locais sem essa correlacao continuam validos no MQTT,
mas nao sao associados a um comando da VPS. A VPS deduplica status por
`(gateway_id, status_id)`.

## Codigos gRPC

| Codigo | Significado | Acao do gateway |
| --- | --- | --- |
| `INVALID_ARGUMENT` | Protobuf, JSON ou campo invalido. | Mantem, registra e aplica cooldown. |
| `UNAUTHENTICATED` / `PERMISSION_DENIED` | Certificado, identidade ou autorizacao invalida. | Mantem e requer correcao operacional. |
| `ALREADY_EXISTS` | Duplicata com conteudo divergente. | Mantem e alerta; nunca sobrescreve. |
| `RESOURCE_EXHAUSTED` | Limite de taxa/tamanho temporario. | Retenta, respeitando `retry-after-ms`. |
| `FAILED_PRECONDITION` / `OUT_OF_RANGE` | Politica ou estado invalido. | Mantem e aplica cooldown. |
| `UNAVAILABLE` / `DEADLINE_EXCEEDED` | Falha temporaria. | Retenta com backoff. |

Respostas e erros de autenticacao nunca revelam detalhes internos. A VPS pode
auditar IDs e transicoes, mas nao payloads nem parametros completos.

## Observabilidade

O diagnostico local futuro deve expor somente metadados: ultimo sucesso,
mensagens e lotes enviados/confirmados/falhos, backoff, idade do item mais
antigo, contadores de comandos e ultimo codigo gRPC. Nunca expor payloads,
topicos, dispositivos ou segredos. Na VPS, as metricas sao agregadas por
gateway: ultima atividade, atraso da outbox, deduplicacoes, autenticacoes
falhas e comandos pendentes/expirados.

## Compatibilidade e rollout

A outbox atual ja persiste antes de enviar, mantem `message_id`, metadados de
dispositivo/topico/tipo, prioridade e ordem de insercao. Ela tambem possui
leitura ordenada, lease, ACK explicito, exclusao apos ACK e reagendamento. O
cliente gRPC, a politica de backoff e o ledger de comandos ainda faltam. Isso
nao altera a semantica MQTT local.

Ordem de entrega:

1. Publicar este contrato e gerar codigo Go a partir do `.proto`.
2. Criar testes de serializacao e contrato com um servidor gRPC falso.
3. Integrar o cliente gRPC a lease, ACK e retentativa; testar sem WireGuard.
4. Implantar VPS de ingestao, WireGuard, CA e certificados por gateway.
5. Ativar dados e testar queda da VPS, deadline, ACK perdido e deduplicacao.
6. Implementar ledger e polling de comandos para um dispositivo de teste.
7. Habilitar comandos apenas para dispositivos com `commands_from_vps: true`.
8. Testar reboot entre `RECEIVED`, `PUBLISHED` e `RESULT`, inclusive rotacao
   de certificados.
