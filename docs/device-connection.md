# Conexão de dispositivos

Este documento descreve como um ESP32 sai da caixa (energizado, sem estado)
até trocar mensagens MQTT válidas com o gateway: registro na rede Wi-Fi,
descoberta do broker e a conexão MQTT propriamente dita. Ele **não** repete o
contrato de tópicos/payloads (ver [mqtt.md](mqtt.md)) nem o cadastro
declarativo no YAML do gateway (ver [configuration.md](configuration.md)) —
apenas conecta essas peças na ordem em que acontecem no dispositivo. O
processo operacional passo a passo para registrar um dispositivo novo está em
[device-onboarding.md](device-onboarding.md).

## Ciclo de vida da conexão

```text
1. Boot do ESP32
2. Associação Wi-Fi (WPA2, credenciais fixas no firmware)
3. Resolução do broker (mDNS: <hostname-orangepi>.local)
4. Conexão MQTT (credenciais próprias do dispositivo, LWT configurado)
5. Inscrição em `devices/<id>/command`
6. Publicação em `devices/<id>/state` (retained) refletindo o estado atual
```

Se qualquer etapa falhar, o dispositivo tenta novamente a partir dela; não há
persistência de fila no ESP32 — mensagens não entregues enquanto o
dispositivo está desconectado são perdidas do lado dele (a durabilidade fim a
fim é responsabilidade da outbox do gateway, ver [queue.md](queue.md), para o
sentido dispositivo→VPS; no sentido gateway→dispositivo local não há retry
automático ainda — ver "Limitações conhecidas").

## Etapa 1 — Registro na rede Wi-Fi

- Credenciais Wi-Fi (SSID/senha) ficam fixas em `secrets.h`, compiladas no
  binário — uma rede por build, sem portal de provisionamento neste momento.
  Trocar de rede ou reprovisionar exige reflash.
- Não há anúncio de hostname DHCP dedicado nem lógica de reconexão com
  backoff nesta etapa; o comportamento inicial é o mesmo já usado em
  `esp32c3-led` hoje (`WiFi.begin` bloqueante). Backoff exponencial e
  provisionamento via portal cativo (como já existe no projeto irmão
  `hardware-monitor`) ficam como evolução futura, fora do escopo agora.

## Etapa 2 — Descoberta do broker (resolução, não DHCP clássico)

O Orange Pi que roda o Mosquitto tem **IP fixo configurado localmente nele
mesmo** (não uma reserva DHCP no roteador — não há acesso à configuração do
roteador nesta rede). Isso significa que o IP é estável do ponto de vista do
Orange Pi, mas os dispositivos ainda precisam de um jeito de encontrá-lo sem
hardcodar esse IP em todo firmware.

**Mecanismo escolhido: mDNS.** O Orange Pi publica seu hostname via
`avahi-daemon`, e cada ESP32 resolve `<hostname>.local` em runtime
(`ESPmDNS` no Arduino core) antes de abrir a conexão MQTT.

- No Orange Pi: `avahi-daemon` precisa estar instalado e habilitado (Armbian
  minimal não traz por padrão). O hostname do sistema operacional é o nome
  publicado — recomenda-se alinhá-lo ao `gateway.id` da configuração (ex.:
  `orangepi-lab-01`), para que `orangepi-lab-01.local` resolva de forma
  previsível. Isso ainda precisa ser configurado; não há automação de deploy
  para isso hoje (ver nota em [deployment.md](deployment.md)).
- No ESP32: resolve o hostname uma vez após conectar ao Wi-Fi e antes de
  chamar `connect()` no cliente MQTT. Se a resolução falhar, tenta de novo
  com um intervalo fixo (não há backoff exponencial nesta etapa, mesma
  decisão da Etapa 1).

**Risco conhecido e sem correção prevista:** como não há acesso à
configuração do roteador, não é possível garantir que multicast (base do
mDNS) não seja bloqueado por AP isolation ou outra política do roteador. Não
há um plano B automático no firmware. Se o mDNS não resolver em campo, a
alternativa manual é hardcodar o IP atual do Orange Pi em `secrets.h` como
contorno pontual — documentado aqui como o procedimento de emergência, não
como caminho suportado.

## Etapa 3 — Conexão MQTT e troca de mensagens

- **Credenciais:** cada dispositivo tem usuário/senha MQTT próprios (nunca a
  credencial do gateway), provisionados manualmente por enquanto — processo
  detalhado em [device-onboarding.md](device-onboarding.md). Sem TLS
  (`mqtt://`, não `mqtts://`): a LAN é considerada confiável no MVP, conforme
  [security.md](security.md); isso evita o custo de RAM/CPU do TLS no
  ESP32-C3 e a gestão de certificados agora.
- **Last Will and Testament (LWT):** configurado na conexão MQTT
  (`will_topic` = tópico `state` do dispositivo, `will_qos = 1`,
  `will_retain = true`). Se o dispositivo cair sem desconexão limpa, o
  broker publica automaticamente o payload de LWT no tópico `state`
  (retained), sobrescrevendo o último estado conhecido com um indicador de
  offline. Isso não está em [mqtt.md](mqtt.md) hoje porque é um mecanismo do
  protocolo MQTT, não um payload publicado pelo dispositivo — vale
  referenciar aqui.
- **Tópicos, payloads, QoS e retenção:** seguem exatamente o contrato de
  [mqtt.md](mqtt.md). Um dispositivo não precisa declarar os cinco tópicos
  possíveis — o gateway aceita qualquer subconjunto não vazio (ver
  `internal/config/config.go`), então um dispositivo simples pode declarar
  só `state` e `command`.
- Após reconectar (Wi-Fi ou MQTT caiu e voltou), o dispositivo reassina
  `command` normalmente como parte do fluxo de `connect()`. Não há lógica de
  "replay" de comandos perdidos durante a queda — ver "Limitações
  conhecidas".

## Limitações conhecidas / decisões adiadas

Registradas aqui para não se perderem, e revisadas quando a robustez virar
prioridade:

- **Relógio sem NTP:** o ESP32-C3 não tem RTC e, por decisão explícita, não
  sincroniza hora via NTP nesta etapa. O `timestamp` RFC3339 exigido por
  [mqtt.md](mqtt.md) é preenchido a partir de época 0 + tempo de boot
  (`millis()`), produzindo uma string sintaticamente válida mas
  semanticamente incorreta (ex.: `1970-01-01T00:00:42Z`). O gateway
  (`internal/mqtt/gateway.go`) só valida formato RFC3339 com sufixo `Z`, não
  plausibilidade — portanto esses payloads são aceitos. Isso significa que
  `timestamp` não é confiável para ordenação ou auditoria enquanto NTP não
  for implementado.
- **Sem reconexão com backoff exponencial** em Wi-Fi ou MQTT (decisão desta
  etapa: ignorar o padrão já existente em `cyd-monitor` por enquanto).
- **Sem redelivery de comandos perdidos** enquanto o dispositivo estava
  offline — mitigado apenas pela camada de outbox/retry que já existe para o
  sentido dispositivo→VPS, não para gateway→dispositivo local.
- **Provisionamento de credenciais MQTT manual** (`mosquitto_passwd`), sem
  automação.
- **Sem TLS local.**
