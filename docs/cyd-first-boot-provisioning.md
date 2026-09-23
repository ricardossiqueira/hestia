# Provisionamento inicial do CYD

Este e o primeiro fluxo sem credencial MQTT compilada no firmware. Ele vale
somente para `cyd-monitor`; LEDs e os demais devices continuam no fluxo atual
ate adotarem a mesma API de primeiro boot.

## Contrato

Um CYD novo contem somente as credenciais Wi-Fi em `src/secrets.h`. Depois de
conectar na rede, ele imprime no Serial Monitor:

```text
Provisioning pending: IP 192.168.15.42, device cyd-monitor-a1b2c3,
endpoint http://192.168.15.42:8080
```

Enquanto nao ha configuracao MQTT valida no NVS, o firmware atende:

- `GET /v1/device-info`: modelo, estado `unprovisioned`, identificador de
  bootstrap e MAC, sem qualquer segredo;
- `POST /v1/provision`: configuracao MQTT enviada exclusivamente pelo processo
  `iot-gateway admin`.

Depois de gravar todos os campos no NVS, o firmware grava por ultimo o marcador
`ready`, responde `201`, fecha o servidor HTTP e passa a conectar ao MQTT.
Uma queda de energia antes do marcador deixa o device em modo de
provisionamento, em vez de usar credenciais parciais.

## Operacao

1. Grave o firmware com apenas Wi-Fi em `src/secrets.h`.
2. Abra o Serial Monitor e aguarde o IP privado exibido pelo CYD.
3. Em `gateway-web`, abra **Dispositivos > Novo dispositivo**, selecione
   **CYD Monitor**, informe um ID estavel e o IP mostrado no serial.
4. `gateway-web` chama `DeviceAdminService.ProvisionCYD`; o browser nunca
   chama o ESP diretamente e nunca recebe a senha MQTT.
5. O gateway valida que o IP responde como `cyd-monitor` nao provisionado,
   cria a role/identidade DynSec, grava a politica no SQLite como desabilitada,
   entrega a configuracao ao CYD, recebe o ACK e habilita a identidade e o
   registry sem reiniciar gateway ou Mosquitto.

Se a entrega chegou ao CYD mas uma etapa de ativacao posterior falhar, o
registro continua desabilitado propositalmente. Use `SetDeviceEnabled` para
recuperar: a senha nao e rotacionada e nao precisa ser reenviada ao NVS.

## Configuracao do Orange Pi

O processo admin precisa saber qual host MQTT o ESP consegue alcancar. Isso nao
pode ser inferido de `mqtt.url`: no Orange Pi ela normalmente e
`127.0.0.1:1884`, que seria loopback do ESP se fosse gravada no NVS. Adicione
ao arquivo central `/etc/iot-gateway/admin-environment`:

```ini
IOT_GATEWAY_DEVICE_MQTT_HOST=192.168.15.195
```

A porta e derivada de `mqtt.url` (`1884` na migracao DynSec). O valor e um
dado de rede, nao um segredo. Reinicie apenas o servico admin depois da
alteracao:

```bash
sudo systemctl restart iot-gateway-admin.service
```

## Limites do MVP

Este e um pareamento de LAN confiavel: nao ha token, QR ou TLS no endpoint do
CYD. O gateway aceita somente IPv4 privados, recusa redirects e verifica modelo
e estado antes de enviar uma configuracao. O endpoint do device existe apenas
antes da primeira configuracao. Antes de usar fora de uma LAN confiavel, a
proxima etapa obrigatoria e TLS para MQTT e pareamento autenticado para a API
temporaria do ESP.
