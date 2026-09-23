# Documentação

| Documento | Conteúdo |
| --- | --- |
| [runtime-registry.md](runtime-registry.md) | Proposta para substituir cadastro YAML por registry SQLite, aplicacao dinamica e provisionamento recuperavel. |
| [architecture.md](architecture.md) | Componentes, fluxos e limites de responsabilidade. |
| [configuration.md](configuration.md) | Cadastro declarativo de dispositivos e referência YAML. |
| [mqtt.md](mqtt.md) | Convenções de tópicos, mensagens e entrega. |
| [device-connection.md](device-connection.md) | Como um ESP32 se conecta: rede Wi-Fi, descoberta do broker e MQTT. |
| [device-onboarding.md](device-onboarding.md) | Checklist operacional para cadastrar um dispositivo novo. |
| [cyd-first-boot-provisioning.md](cyd-first-boot-provisioning.md) | Fluxo MVP para gravar credenciais MQTT diretamente no NVS do CYD. |
| [mosquitto-device-provisioning.md](mosquitto-device-provisioning.md) | Como conceder credencial/ACL MQTT a um dispositivo no Mosquitto. |
| [queue.md](queue.md) | Outbox SQLite e comportamento durante falhas de rede. |
| [uplink-v1.md](uplink-v1.md) | Contrato gRPC/Protobuf futuro entre o gateway e a VPS. |
| [api-v1.md](api-v1.md) | Contrato Connect-RPC da API local: comandos, descoberta de schema e status. |
| [security.md](security.md) | Limites de acesso e gestão de segredos. |
| [deployment.md](deployment.md) | Premissas para executar no Orange Pi. |
| [implementation-plan.md](implementation-plan.md) | Marcos incrementais de construção. |
| [decisions.md](decisions.md) | Decisões de arquitetura já assumidas. |
