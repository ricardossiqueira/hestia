# Configuração declarativa

O arquivo principal será mantido localmente no Orange Pi, por padrão em `config/gateway.yaml`. Ele é a fonte de verdade dos dispositivos aceitos e das permissões de encaminhamento.

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
  max_outbox_age: 168h

devices:
  - id: esp32-sala
    type: esp32
    enabled: true
    topics:
      telemetry: devices/esp32-sala/telemetry
      state: devices/esp32-sala/state
      event: devices/esp32-sala/event
      command: devices/esp32-sala/command
      command_result: devices/esp32-sala/command-result
    forwarding:
      telemetry_to_vps: true
      state_to_vps: true
      events_to_vps: true
      commands_from_vps: true
```

## Regras de validação planejadas

- `gateway.id` e cada `devices[].id` são obrigatórios e únicos.
- Todos os tópicos devem começar com `devices/<device-id>/`.
- Um tópico não pode pertencer a mais de um dispositivo.
- Dispositivos desabilitados não recebem nem originam tráfego encaminhado.
- A outbox deve ter limites de quantidade e idade para proteger o armazenamento.
- Mudanças no YAML serão aplicadas por reinício no MVP; recarga sem reinício é uma melhoria futura.

## Execução MQTT local

Antes de iniciar, valide a configuração e exporte as credenciais configuradas no YAML:

```bash
iot-gateway validate --config config/gateway.yaml
export MQTT_GATEWAY_USERNAME=gateway
export MQTT_GATEWAY_PASSWORD='senha-local'
iot-gateway run --config config/gateway.yaml
```

O processo assina somente os tópicos inbound (`telemetry`, `state`, `event` e `command_result`) dos dispositivos com `enabled: true`. O tópico `command` é exclusivamente de saída. Para testar a rota de comando, use `iot-gateway publish-test-command --config config/gateway.yaml --device esp32-sala`; ele cria um comando `gateway_test` com UUID novo, QoS 1 e `retain=false`.

IDs sao unicos e os topicos seguem a rota canonica exata `devices/<device-id>/<tipo>`. Assim, a unicidade dos topicos entre dispositivos decorre diretamente dos IDs unicos; nao ha topicos genericos nem curingas na configuracao.

`devices[].enabled` e obrigatorio, inclusive quando o dispositivo estiver desabilitado.

## Rotas locais genéricas

Rotas conectam tópicos de dispositivos já declarados, sem associar o gateway a modelos ou funções específicas de hardware. A origem precisa ser um tópico inbound habilitado (`telemetry`, `state`, `event` ou `command_result`) e o destino precisa ser um tópico `command` habilitado.

```yaml
routes:
  - id: status-para-display
    source_topic: devices/orangepi-monitor/telemetry
    destination_topic: devices/cyd-monitor/command
    transform:
      type: json_command
      command_type: render_system_status
    qos: 1
    retain: false
```

`json_command` é uma transformação genérica: ela preserva o objeto JSON de origem em `parameters`, cria um novo `command_id` UUID v4 e define `type` pelo valor declarativo de `command_type`. O gateway não contém conhecimento de `orangepi-monitor`, `cyd-monitor` ou qualquer tipo de comando concreto.

## Cadastro de um ESP32

1. Escolha um ID estável, por exemplo `esp32-sala`.
2. Adicione sua definição ao YAML.
3. Configure o firmware com o ID e o endereço IP/nome local do Mosquitto.
4. Reinicie o gateway após validar a configuração.
5. Confirme no log que o gateway assinou os tópicos previstos.
