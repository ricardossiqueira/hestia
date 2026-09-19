# Arquitetura

## Princípio

O gateway é **offline-first**: a rede externa não é necessária para que ESP32 e Orange Pi troquem mensagens localmente. Quando existir uma VPS, a indisponibilidade dela não pode bloquear nem apagar eventos locais.

## Componentes

```text
┌─────────┐     MQTT      ┌───────────┐     MQTT      ┌─────────────┐
│ ESP32 A │ ────────────> │ Mosquitto │ <───────────> │ Gateway Go  │
└─────────┘               │ Orange Pi │               └──────┬──────┘
┌─────────┐               └───────────┘                      │
│ ESP32 B │ ──────────────────────────────────────────────────┤
└─────────┘                                                    │
                                                          ┌────▼────┐
                                                          │ SQLite  │
                                                          │ outbox  │
                                                          └────┬────┘
                                                               │
                                                     futuro: WireGuard
                                                               │
                                                            ┌──▼───┐
                                                            │ VPS  │
                                                            └──────┘
```

- **ESP32:** publicam telemetria/estado e assinam comandos.
- **Mosquitto:** broker MQTT local. Ele não interpreta a política de dispositivos.
- **Gateway Go:** lê o YAML, valida tópicos e payloads, associa mensagens ao dispositivo e encaminha dados autorizados.
- **SQLite:** fila persistente de saída e, posteriormente, histórico operacional mínimo.
- **Uplink:** adaptador ainda não implementado para enviar e receber dados da VPS pelo túnel WireGuard.

## Fluxos

### Telemetria local para a VPS

1. Um ESP32 publica em `devices/<id>/telemetry`.
2. O gateway confere se o dispositivo e o tópico são declarados e habilitados.
3. O evento é normalizado e gravado na outbox SQLite em uma transação.
4. O uplink futuro envia o evento quando estiver disponível e confirma a entrega.

### Comando da VPS para o ESP32

1. A VPS envia um comando endereçado a um `device_id` pelo uplink autenticado.
2. O gateway confirma que `commands_from_vps` está habilitado para aquele dispositivo.
3. O gateway publica em `devices/<id>/command` no broker local.
4. Se o ESP32 publicar resultado, o gateway o relaciona ao comando e o encaminha como evento.

## Escopo de roteamento

O gateway não encaminha MQTT genericamente entre redes. Ele interpreta mensagens conhecidas e encaminha eventos e comandos conforme a política YAML. Isso reduz a superfície de segurança e impede que um dispositivo local publique em tópicos de outro dispositivo.
