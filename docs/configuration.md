# Configuração declarativa

O arquivo principal será mantido localmente no Orange Pi, por padrão em `config/gateway.yaml`. Ele declara identidade do gateway, conexão MQTT, armazenamento, diagnóstico e a API opcional - devices e automações vivem no registry SQLite, administrados via `gateway-web`/`DeviceV2API` (`internal/apigateway/device_v2.go`), não neste arquivo.

Credenciais e chaves **não** devem entrar em arquivos versionados. O YAML referencia variáveis de ambiente ou caminhos de arquivos locais protegidos.

`gateway.timezone` e opcional. Quando omitido, o gateway opera internamente em UTC; informe uma zona IANA, como `America/Sao_Paulo`, apenas quando ela for necessaria para a apresentacao operacional.

`mqtt.url` deve usar o esquema `mqtt` ou `mqtts` e declarar a porta explicitamente, por exemplo `mqtt://127.0.0.1:1883`.

`mqtt.username_env` e `mqtt.password_env` são obrigatórios e devem ser declarados juntos. Os dois valores precisam existir e não podem estar vazios no ambiente do processo. O gateway nunca grava esses valores em logs.

## Exemplo

```yaml
gateway:
  id: orangepi-lab-01
  timezone: America/Sao_Paulo

mqtt:
  url: mqtt://127.0.0.1:1883
  client_id: iot-gateway-orangepi-lab-01
  username_env: MQTT_GATEWAY_USERNAME
  password_env: MQTT_GATEWAY_PASSWORD

storage:
  sqlite_path: /var/lib/iot-gateway/gateway.db
  max_outbox_messages: 10000
  max_outbox_bytes: 33554432
  max_outbox_age: 168h

diagnostics:
  address: 127.0.0.1:8080
  request_timeout: 2s
```

## Regras de validação

- `gateway.id` é obrigatório.
- A outbox deve ter limites de quantidade, bytes e idade para proteger o armazenamento.

## Execução MQTT local

Antes de iniciar, valide a configuração e exporte as credenciais configuradas no YAML:

```bash
iot-gateway validate --config config/gateway.yaml
export MQTT_GATEWAY_USERNAME=gateway
export MQTT_GATEWAY_PASSWORD='senha-local'
iot-gateway run --config config/gateway.yaml
```

O processo assina os tópicos dos devices V2 ativos no registry (`devices/<device-id>/<canal>`), carregados por `EnableV2Runtime` a partir das manifest bindings - ver `internal/registry`/`internal/devicev2`. Não há mais tópicos declarados neste YAML.

## Diagnóstico operacional local

`diagnostics` é opcional. Quando omitido, o gateway escuta em
`127.0.0.1:8080` e limita cada requisição a `2s`. `diagnostics.address` deve
ser um IP literal de loopback (`127.0.0.1` ou `::1`) com porta entre 1 e 65535;
nomes, `0.0.0.0` e interfaces da LAN são recusados. O limite de
`request_timeout` é 10 segundos.

Os únicos endpoints são locais e aceitam apenas `GET`:

- `/healthz`: retorna `200` quando o gateway iniciou e a conexão MQTT está ativa; caso contrário, `503`.
- `/status`: retorna estado MQTT, contadores desde o boot e uma visão lógica da outbox (`messages`, `payload_bytes` e o registro pendente mais antigo). Mensagens expiradas não entram nesses números; a consulta não altera a SQLite.

Não são expostos IDs de dispositivos, tópicos, payloads, credenciais ou erros internos. Por exemplo, no Orange Pi:

```bash
curl --fail http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/status
```

## Rotas locais (removidas)

O modelo de rotas locais (`routes:` neste YAML, conectando o tópico inbound de
um device V1 ao tópico `command` de outro) foi removido junto com o resto do
V1 - ver a ADR de remoção em [decisions.md](decisions.md). Não há ainda um
equivalente para devices V2; a direção combinada é tratar tudo como device V2
(incluindo `orangepi-monitor`/`cyd-monitor`, hoje ainda não migrados) e usar
automações para ligar uma saída a um comando, não um conceito de rota
separado.

## Cadastro de um ESP32

Checklist completo (credencial MQTT, ACL, YAML, firmware, verificação) em
[device-onboarding.md](device-onboarding.md). O mecanismo de conexão em si
(Wi-Fi, descoberta do broker, MQTT) está em
[device-connection.md](device-connection.md). Ambos documentam o fluxo V1 -
o onboarding de devices V2 é feito por descoberta/pareamento via
`gateway-web`, não por edição manual do YAML.
