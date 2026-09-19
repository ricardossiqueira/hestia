# MQTT

## Broker

O Mosquitto será executado no Orange Pi. O gateway Go será um cliente MQTT, assim como os ESP32.

## Tópicos por dispositivo

```text
devices/<device-id>/telemetry
devices/<device-id>/state
devices/<device-id>/event
devices/<device-id>/command
devices/<device-id>/command-result
```

- `telemetry`: medições periódicas, como temperatura ou nível de bateria.
- `state`: estado atual publicado pelo dispositivo, preferencialmente retained.
- `event`: ocorrências pontuais, como botão pressionado ou alerta.
- `command`: pedidos do gateway para o dispositivo.
- `command-result`: resposta do dispositivo a um comando.

## Payloads

No MVP, mensagens devem ser JSON UTF-8. Todo payload deve incluir `timestamp` em UTC e `message_id` UUID, permitindo deduplicação futura.

```json
{
  "message_id": "b4a5bb31-1710-4f7b-a043-1b6a292d04ad",
  "timestamp": "2026-09-18T15:00:00Z",
  "temperature_c": 24.6,
  "humidity_pct": 61.2
}
```

Exemplo de comando:

```json
{
  "command_id": "a9f2290d-d1ee-4cbc-841d-03e29a7f028c",
  "type": "set_output",
  "parameters": { "pin": 2, "value": true }
}
```

## Qualidade de serviço

- Telemetria: QoS 1 no MVP para reduzir perdas sem complexidade excessiva.
- Estado: QoS 1 e `retain=true` no firmware do ESP32.
- Comandos e resultados: QoS 1.

QoS 1 entrega mensagens pelo menos uma vez. Por isso, `message_id` e `command_id` são importantes: consumidores devem tolerar duplicatas.
