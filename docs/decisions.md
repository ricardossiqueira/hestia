# Decisões de arquitetura

| ID | Decisão | Motivo |
| --- | --- | --- |
| ADR-001 | Go no gateway | Binário único, baixo uso de recursos e boa concorrência. |
| ADR-002 | Mosquitto como broker local | Maduro, pequeno e separa broker da lógica de domínio. |
| ADR-003 | YAML como cadastro de dispositivos | Fácil de revisar e adequado ao conjunto inicial pequeno e estável. |
| ADR-004 | SQLite como outbox | Persistente, sem serviço adicional e apropriado ao Orange Pi. |
| ADR-005 | Sem regras locais no MVP | Mantém o escopo no transporte confiável de dados e comandos. |
| ADR-006 | WireGuard somente para a fase VPS | A rede externa não deve atrasar a validação do gateway local. |
| ADR-007 | Entrega pelo menos uma vez | Mais simples e robusta; deduplicação via IDs. |
| ADR-008 | UI de admin em serviço systemd separado, não no gateway | `iot-gateway.service` roda sandboxed (`ProtectSystem=strict`, `ReadWritePaths` só a outbox) de propósito; registrar dispositivo exige escrever `/etc/mosquitto/*` e `gateway.yaml` e chamar `systemctl`, o que o processo do gateway nunca deve poder fazer. Mesmo motivo do updater já ser um serviço root separado. |
