# Plano de implementação

## Marco 0 — Fundação (concluído)

- Estrutura inicial do repositório.
- Documentação de arquitetura e contratos MQTT/YAML.

## Marco 1 — Configuração

- Definir structs Go e leitura de YAML.
- Validar IDs, tópicos e duplicidades.
- Criar comando de validação (`iot-gateway validate --config ...`).
- Testes unitários da validação.

## Marco 2 — MQTT local (concluído)

- Cliente MQTT de produção baseado em Paho, com credenciais lidas apenas de variáveis de ambiente.
- Assinatura QoS 1 somente de `telemetry`, `state`, `event` e `command-result` dos dispositivos habilitados.
- Validação de JSON UTF-8, objeto, `message_id` UUID e `timestamp` RFC3339 em UTC antes do registro.
- Logs estruturados de aceitação/rejeição sem payload ou segredos.
- Publicação QoS 1, não retained, de comandos validados para dispositivos habilitados.
- Comandos `iot-gateway run --config <arquivo>` e `iot-gateway publish-test-command --config <arquivo> --device <id>`.
- Testes unitários com cliente MQTT falso; nenhum Mosquitto é necessário para a suite.

## Extensão local — Rotas declarativas (concluído)

- Rotas MQTT entre endpoints de dispositivos habilitados e declarados.
- Transformação genérica `json_command`, configurada inteiramente por YAML.
- QoS e retenção definidos por rota; sem conhecimento de domínios de dispositivos no gateway.

## Extensão local — UI de admin (concluído)

- Serviço systemd separado e privilegiado (`iot-gateway-admin.service`),
  fora do sandbox do gateway (ADR-008 em `decisions.md`).
- Registro, listagem e remoção de dispositivos via web: credencial/ACL no
  Mosquitto (chamando `deploy/mosquitto-provision-device.sh`), edição do
  `gateway.yaml` preservando comentários/formatação, restart do gateway.
- Autenticação HTTP Basic com credencial única vinda de variáveis de
  ambiente; sem TLS — uso restrito à LAN confiável.

## Marco 3 — Outbox SQLite

- Criar schema e repositório da fila.
- Persistir telemetria, estado e eventos destinados ao uplink.
- Implementar limites e métricas da fila.

## Marco 4 — Diagnóstico e operação

- Endpoint HTTP local de saúde e status.
- Logs estruturados e configuração de systemd.
- Guia para Mosquitto e primeiro ESP32.

## Marco 5 — Uplink VPS

- Configurar WireGuard fora do binário.
- Definir contrato autenticado gateway–VPS.
- Implementar consumidor/produtor de outbox e entrada de comandos.
- Testar desconexão, duplicação e recuperação.
