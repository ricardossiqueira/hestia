# IoT Gateway

Gateway IoT leve, escrito em Go e destinado inicialmente a um Orange Pi Zero 2W com 1 GB de RAM.

Ele recebe eventos de ESP32 pela rede local via MQTT, aplica uma política declarada em YAML, mantém uma fila persistente para encaminhamento externo e, em uma fase futura, troca dados com uma VPS por um túnel WireGuard.

> Estado: fundação e documentação de arquitetura. A implementação do gateway ainda será construída por etapas.

## Objetivos do MVP

- Operar como cliente MQTT contra um broker local Mosquitto no Orange Pi.
- Registrar dispositivos conhecidos por meio de um arquivo YAML.
- Aceitar telemetria e estados apenas nos tópicos declarados.
- Manter uma outbox persistente em SQLite para eventos destinados à VPS.
- Receber comandos do uplink futuro e publicá-los ao ESP32 correto.
- Expor saúde e diagnóstico básicos localmente.

## Não objetivos iniciais

- Motor de regras e automações locais.
- Descoberta automática de dispositivos.
- Interface web.
- Broker MQTT implementado em Go (o broker será o Mosquitto).
- Comunicação com VPS e WireGuard: previstos, mas fora do primeiro marco de implementação.

## Arquitetura

```text
ESP32 ── MQTT/Wi-Fi ──> Mosquitto local ──> Gateway Go ──> SQLite outbox
                                                     └──> VPS, futuramente

VPS ── WireGuard, futuramente ──> Gateway Go ──> Mosquitto local ──> ESP32
```

Leia a documentação em [`docs/`](docs/README.md), começando por [arquitetura](docs/architecture.md) e [configuração](docs/configuration.md).

## Estrutura

```text
cmd/gateway/         executável principal
configs/             exemplos de configuração não secreta
docs/                decisões e guias do projeto
internal/            código privado do gateway (a ser implementado)
migrations/          schema SQLite futuro
deploy/              unidades systemd e material de implantação futuro
```

## Próximo marco

Implementar o processo Go mínimo que lê e valida o YAML, conecta ao Mosquitto local e registra os dispositivos configurados. Consulte o [plano de implementação](docs/implementation-plan.md).
