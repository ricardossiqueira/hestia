# Cadastro de um dispositivo novo

Checklist operacional para registrar um ESP32 novo de ponta a ponta: broker,
gateway e firmware. Pressupõe que o mecanismo de conexão (Wi-Fi, descoberta
do broker, MQTT) já é conhecido — ver [device-connection.md](device-connection.md)
para o *porquê* de cada peça. Este documento substitui a seção "Cadastro de
um ESP32" antes existente em [configuration.md](configuration.md).

`gateway-web` (via `DeviceAdminService`, ver `docs/api-v1.md` e
`deploy/README.md`) faz os passos 2 e 3 abaixo de uma vez (credencial/ACL
no Mosquitto, entrada no `gateway.yaml`, restart do serviço) e é o caminho
recomendado no dia a dia. Os passos manuais abaixo continuam valendo para
depuração ou quando o processo de admin está fora do ar.

## 1. Escolher o `device_id`

- ID estável, minúsculo, com hífen (ex.: `esp32c3-led`). Precisa ser único
  entre todos os dispositivos cadastrados no YAML do gateway.
- É fixado no firmware em tempo de build (não é derivado do MAC nem
  configurado em runtime) — decisão desta etapa para manter o firmware
  simples. Um binário por dispositivo.

## 2. Criar a credencial MQTT e a ACL no Mosquitto

Cada dispositivo tem usuário/senha MQTT próprios — nunca reutilize a
credencial do gateway. Nunca conceda `readwrite devices/<id>/#`: declare
apenas os tópicos e direções que o dispositivo realmente usa.

No Orange Pi, como root:

```bash
sudo deploy/mosquitto-provision-device.sh <device_id> <topico>...
```

`<topico>` é um ou mais de `telemetry state event command command-result`
(a direção read/write é derivada do nome). Isso cria/rotaciona a senha,
escreve o bloco de ACL mínimo e recarrega o Mosquitto — imprime no final o
trecho pronto para colar em `secrets.h`. Detalhes de como o script funciona
e o passo a passo manual equivalente estão em
[mosquitto-device-provisioning.md](mosquitto-device-provisioning.md).

## 3. Declarar o dispositivo no YAML do gateway

Adicione a entrada em `config/gateway.yaml` (ver
[configuration.md](configuration.md) para a referência completa de campos).
Um dispositivo só precisa declarar os tópicos que efetivamente usa — não é
obrigatório declarar os cinco:

```yaml
devices:
  - id: esp32c3-led
    type: esp32
    enabled: true
    topics:
      state: devices/esp32c3-led/state
      command: devices/esp32c3-led/command
    forwarding:
      state_to_vps: false
      commands_from_vps: false
```

`forwarding` fica `false` em todos os campos enquanto o uplink VPS não
existir (ver [uplink-v1.md](uplink-v1.md)) — isso não impede a troca local de
mensagens entre ESP32 e gateway, só a fila/envio remoto.

Valide antes de reiniciar o serviço:

```bash
iot-gateway validate --config config/gateway.yaml
```

## 4. Configurar o firmware

Em `secrets.h` do projeto do dispositivo (não versionado; copie de
`secrets.h.example`):

- SSID/senha Wi-Fi.
- Hostname mDNS do Orange Pi (ex.: `orangepi-lab-01.local`) ou, como
  contorno de emergência se o mDNS não resolver, o IP fixo atual.
- `device_id` usado nos tópicos (`devices/<device_id>/...`).
- Usuário/senha MQTT criados no passo 2.

## 5. Gravar e verificar

1. `pio run -t upload` (ou `just flash`, conforme o projeto).
2. Acompanhe o monitor serial: deve mostrar Wi-Fi conectado, hostname
   resolvido e conexão MQTT bem-sucedida.
3. No Orange Pi, reinicie o `iot-gateway` (ou confirme que já está rodando
   com a config validada) e confira no log/journal que ele assinou os
   tópicos do novo dispositivo.
4. `curl --fail http://127.0.0.1:8080/healthz` no Orange Pi continua `200`.
5. Publique/observe manualmente para confirmar a troca de mensagens, por
   exemplo com `iot-gateway publish-test-command --config config/gateway.yaml
   --device esp32c3-led` (se o dispositivo tiver tópico `command`) e
   observando o tópico `state` (retained) com `mosquitto_sub`.

## Checklist resumido

- [ ] `device_id` único escolhido
- [ ] `deploy/mosquitto-provision-device.sh` rodado (credencial + ACL + reload)
- [ ] Entrada adicionada em `gateway.yaml` e validada
- [ ] `secrets.h` do firmware preenchido
- [ ] Firmware gravado e logs conferidos
- [ ] `/healthz` OK e troca de mensagens confirmada manualmente
