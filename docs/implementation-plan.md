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

## Extensão local — endpoint HTTP de comando (concluído, substituído)

- `POST /commands` (`internal/commandapi`), opcional (`commands:` no
  `gateway.yaml`), rodando dentro do próprio processo `iot-gateway run` —
  reaproveita a conexão MQTT já estabelecida em vez de abrir uma nova por
  requisição (ADR-009 em `decisions.md`).
- `command_id` gerado no servidor; `type`/`parameters` vêm do corpo da
  requisição e seguem o contrato já existente em `docs/mqtt.md`.
- Mesma autenticação HTTP Basic e mesma restrição de uso à LAN confiável da
  UI de admin.
- **Substituído** pela API local Connect-RPC abaixo; `internal/commandapi`
  foi removido.

## Extensão local — API local Connect-RPC (concluído)

- `internal/api`, opcional (`api:` no `gateway.yaml`), rodando dentro do
  próprio processo `iot-gateway run` — mesmo motivo de reaproveitar a
  conexão MQTT já estabelecida (ADR-009 em `decisions.md`).
- Transporte Connect-RPC (`connectrpc.com/connect`): gRPC, gRPC-Web e
  HTTP/JSON na mesma porta (ADR-010).
- `DeviceService` (`ListDevices`, `ListDeviceCommands`, `PublishCommand`) e
  `GatewayService` (`GetStatus`), definidos em
  `api/proto/iot/gateway/api/v1/api.proto`.
- `internal/deviceprofile`: registry compilado de profiles de device
  (`led.v1` nesta etapa); valida e canoniza `parameters` contra um schema
  Protobuf antes de publicar; device sem `profile:` cai em fallback opaco
  (ADR-011).
- Mesma autenticação HTTP Basic e mesma restrição de uso à LAN confiável da
  UI de admin.
- Substitui `internal/commandapi`, removido nesta mesma entrega.

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
