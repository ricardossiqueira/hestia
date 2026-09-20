# Provisionamento de dispositivos no Mosquitto

Este e o handoff operacional para conceder acesso MQTT a um dispositivo novo
na rede local. Ele trata somente do broker Mosquitto; o cadastro declarativo
no YAML do gateway continua sendo uma etapa separada.

## Script

Os passos 1-3 abaixo (credencial, ACL, reload) estao automatizados em
`deploy/mosquitto-provision-device.sh`. Rode como root no Orange Pi:

```bash
sudo deploy/mosquitto-provision-device.sh <device_id> <topico>...
# ou: sudo just provision-device <device_id> <topico>...
```

`<topico>` e um ou mais de `telemetry state event command command-result`; a
direcao (leitura/escrita) e derivada do nome, nunca um parametro separado.
Rodar de novo para o mesmo `device_id` rotaciona a senha e substitui o bloco
ACL pelos topicos passados. `--remove <device_id>` revoga a credencial e
remove o bloco ACL. O restante deste documento explica o que o script faz
por baixo dos panos e como fazer manualmente se precisar depurar algo.

A UI de admin (`iot-gateway-admin.service`, ver `deploy/README.md`) chama
este mesmo script por baixo dos panos e ainda escreve o `gateway.yaml` e
reinicia o gateway - use-a no dia a dia; volte a este script quando precisar
depurar algo diretamente no Mosquitto ou quando o serviço de admin estiver
fora do ar.

Exemplo real: `cyd-monitor` recebe comandos do gateway, portanto possui uma
credencial MQTT propria e permissao de **leitura** somente em
`devices/cyd-monitor/command`.

## Principios

- Um dispositivo tem um usuario MQTT exclusivo, igual ao seu `device_id`.
- Nunca reutilize a credencial MQTT do gateway ou de outro dispositivo.
- Conceda somente os topicos e as direcoes necessarias. Nunca use
  `readwrite devices/<id>/#` como atalho.
- O YAML do gateway nao cria usuario nem ACL no Mosquitto; ambos precisam ser
  provisionados aqui.
- Senhas, arquivos `secrets.h` e o arquivo de senhas do Mosquitto nao entram
  no Git nem em logs.

## Pre-requisitos e descoberta dos caminhos reais

No Orange Pi, confirme que o broker esta ativo e descubra os arquivos que a
instalacao em uso referencia. Os caminhos mais comuns sao
`/etc/mosquitto/passwd` e um arquivo ACL em `/etc/mosquitto/`, mas a
configuracao instalada e a fonte de verdade.

```bash
sudo systemctl status mosquitto.service --no-pager
sudo grep -R -nE '^(allow_anonymous|password_file|acl_file|listener)' /etc/mosquitto
```

O resultado deve apontar para:

```text
allow_anonymous false
password_file /caminho/para/passwd
acl_file /caminho/para/acl
```

Nos exemplos seguintes, substitua `PASSWORD_FILE`, `ACL_FILE` e `DEVICE_ID`
pelos valores reais. Use um ID estavel, em minusculas, como `cyd-monitor` ou
`esp32c3-led`.

## 1. Criar a credencial

Gere uma senha aleatoria e guarde-a no arquivo local de segredos do firmware.
Use o modo interativo para que a senha nao apareca no historico do shell nem
na lista de processos.

```bash
openssl rand -base64 24
sudo mosquitto_passwd PASSWORD_FILE DEVICE_ID
```

O segundo comando pede a senha duas vezes. Por exemplo, para o CYD:

```bash
sudo mosquitto_passwd /etc/mosquitto/passwd cyd-monitor
```

Depois, copie a senha somente para o `secrets.h` local do projeto do
dispositivo. O usuario e a senha serao usados pelo cliente MQTT no firmware.

## 2. Adicionar a ACL minima

Edite `ACL_FILE` com `sudoedit`. Cada bloco inicia com `user <device_id>`.
Declare explicitamente cada topico e direcao que o firmware usa.

### Dispositivo que somente recebe comandos

Este e o caso atual de `cyd-monitor`:

```text
user cyd-monitor
topic read devices/cyd-monitor/command
```

### Dispositivo que somente publica dados

Exemplo de sensor que publica telemetria e estado:

```text
user sensor-sala
topic write devices/sensor-sala/telemetry
topic write devices/sensor-sala/state
```

### Dispositivo que publica dados e recebe comandos

Adicione apenas os topicos realmente declarados no YAML:

```text
user esp32c3-led
topic read devices/esp32c3-led/command
topic write devices/esp32c3-led/state
topic write devices/esp32c3-led/command-result
```

Topicos possiveis de escrita do dispositivo sao `telemetry`, `state`, `event`
e `command-result`. O topico `command` e de leitura para o dispositivo e de
escrita para a identidade MQTT do gateway.

Nao conceda leitura de `telemetry`, `state`, `event` ou `command-result` a um
dispositivo, nem escrita em `command`, salvo se uma necessidade concreta tiver
sido revisada. Esse limite impede que um firmware comprometido finja ser o
gateway ou leia dados de outros componentes.

## 3. Recarregar e verificar o broker

Recarregue a configuracao apos alterar senha ou ACL e verifique o journal:

```bash
sudo systemctl reload mosquitto.service
sudo systemctl status mosquitto.service --no-pager
sudo journalctl -u mosquitto.service -n 30 --no-pager
```

Se a unidade instalada nao suportar `reload`, use `restart` somente depois de
revisar o arquivo ACL; isso interrompe clientes MQTT brevemente:

```bash
sudo systemctl restart mosquitto.service
```

## 4. Testar a permissao concedida

Teste com as credenciais do dispositivo em um terminal separado. Para o CYD,
ele deve conseguir assinar somente o topico de comando. Leia a senha sem
ecoar seu valor e descarte a variavel ao terminar a sessao:

```bash
read -rs MQTT_CYD_PASSWORD; echo
mosquitto_sub -h 127.0.0.1 -p 1883 \
  -u cyd-monitor -P "$MQTT_CYD_PASSWORD" \
  -t devices/cyd-monitor/command -d
```

Em outro terminal, publique um comando de teste usando a credencial do
gateway (nao a do CYD). O payload deve obedecer ao contrato em
[mqtt.md](mqtt.md):

```bash
read -rs MQTT_GATEWAY_PASSWORD; echo
mosquitto_pub -h 127.0.0.1 -p 1883 \
  -u gateway -P "$MQTT_GATEWAY_PASSWORD" \
  -t devices/cyd-monitor/command -q 1 \
  -m '{"command_id":"a9f2290d-d1ee-4cbc-841d-03e29a7f028c","type":"render_system_status","parameters":{}}'
```

O `mosquitto_sub` deve receber a mensagem. Tambem faca um teste negativo: a
credencial do CYD nao deve poder publicar em seu proprio topico `command` nem
assinar topicos de outro dispositivo. O broker deve recusar a operacao; use o
modo `-d` e o journal para confirmar.

Para um dispositivo publicador, inverta o teste: publique com a credencial do
dispositivo em um topico de escrita permitido e observe com a credencial do
gateway.

Nao deixe senhas em comandos salvos no historico. Para testes recorrentes,
prefira exportar variaveis em uma sessao temporaria ou executar o teste a
partir de um terminal protegido.

## 5. Concluir o onboarding

Depois que a autenticacao e a ACL funcionarem:

1. Declare o dispositivo e somente seus topicos reais em `gateway.yaml`.
2. Valide a configuracao com `iot-gateway validate --config <arquivo>`.
3. Configure no firmware o mesmo `device_id`, usuario e senha.
4. Confirme nos logs do gateway que os topicos inbound esperados foram
   assinados.

Consulte [device-onboarding.md](device-onboarding.md) para o checklist que
inclui YAML, firmware e verificacao fim a fim.

## Rotacao ou remocao

Para rotacionar uma senha, atualize primeiro o segredo do firmware, depois
substitua a senha existente com `mosquitto_passwd` e recarregue o broker. Planeje
uma janela curta em que o dispositivo sera reconectado.

Para remover um dispositivo: desabilite-o no YAML do gateway, remova seu bloco
ACL, exclua sua credencial e recarregue o broker:

```bash
sudo mosquitto_passwd -D PASSWORD_FILE DEVICE_ID
sudo systemctl reload mosquitto.service
```

Revogue a credencial antes de descartar ou reutilizar o hardware.
