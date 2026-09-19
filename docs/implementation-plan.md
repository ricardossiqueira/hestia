# Plano de implementação

## Marco 0 — Fundação (concluído)

- Estrutura inicial do repositório.
- Documentação de arquitetura e contratos MQTT/YAML.

## Marco 1 — Configuração

- Definir structs Go e leitura de YAML.
- Validar IDs, tópicos e duplicidades.
- Criar comando de validação (`iot-gateway validate --config ...`).
- Testes unitários da validação.

## Marco 2 — MQTT local

- Conectar ao Mosquitto.
- Assinar os tópicos dos dispositivos habilitados.
- Logar mensagens validadas e rejeitadas.
- Publicar um comando de teste.

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
