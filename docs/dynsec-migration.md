# Migração para Mosquitto Dynamic Security

Este procedimento retira a administração diária de devices dos arquivos
`/etc/mosquitto/passwd`, `/etc/mosquitto/acl` e do script legado. O estado
mutável passa a ser: registry no SQLite para política do gateway, e o estado
interno do plugin DynSec para hash de senha, client e role/ACL.

## Arquitetura e limite da transição

O DynSec é global ao processo Mosquitto. Por isso, durante a validação há dois
processos independentes: o legado em `1883` e o DynSec em `1884`; nunca misture
`password_file`/`acl_file` e DynSec em listeners do mesmo processo.

O gateway atual possui uma única conexão MQTT. Logo, `1884` serve para validar
o broker em paralelo, mas os devices só passam a produzir tráfego útil quando
gateway e firmware forem virados para `1884` na mesma janela de mudança. Isso
é uma troca única de infraestrutura; adicionar devices depois dela não causa
restart do gateway nem do broker.

## Instalação inicial no Orange Pi

Crie `/etc/mosquitto-dynsec/mosquitto.conf` com o plugin e um arquivo de estado
em `/var/lib/mosquitto-dynsec/dynamic-security.json`, ambos conforme o
procedimento operacional. No Mosquitto 2.0.x, inicialize uma única vez:

```bash
sudo /usr/bin/mosquitto_ctrl dynsec init \
  /var/lib/mosquitto-dynsec/dynamic-security.json \
  iot-gateway-dynsec-admin
```

O comando pede a senha sem colocá-la na linha de comando. Salve-a em
`/etc/iot-gateway/secrets/dynsec-admin-password`, propriedade `root:root`,
modo `0600`. O arquivo `dynamic-security.json` pertence a `mosquitto:mosquitto`
e deve ter modo `0640`; não o edite para cadastrar devices.

Instale a unidade versionada:

```bash
sudo install -m 0644 deploy/mosquitto-dynsec.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now mosquitto-dynsec.service
```

Valide localmente, sem exibir a senha:

```bash
/usr/bin/mosquitto_ctrl -h 127.0.0.1 -p 1884 \
  -u iot-gateway-dynsec-admin dynsec listClients
```

O aviso de `mosquitto_ctrl` sobre TLS é esperado para essa validação em
`127.0.0.1`: a senha não deixa a máquina. A administração do DynSec pelo
gateway também é sempre loopback. Antes de expor devices fora de uma LAN
confiável, habilite TLS para o listener de devices.

## Serviço do gateway

Depois de publicar uma versão que contém o adaptador DynSec, instale a unidade
`deploy/iot-gateway-admin.service`. Ela não roda mais como root: recebe o
segredo temporariamente por `LoadCredential` e conversa com o broker em
loopback. Para o `EnvironmentFile` continuar legível pelo usuário do serviço:

```bash
sudo chown root:iot-gateway /etc/iot-gateway/admin-environment
sudo chmod 640 /etc/iot-gateway/admin-environment
sudo install -m 0644 deploy/iot-gateway-admin.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl restart iot-gateway-admin.service
```

`IOT_GATEWAY_DYNSEC_URL`, o usuário administrador e o caminho da credencial
estão na unidade, não no YAML. O conteúdo da senha não vai para ambiente,
SQLite, log, flag ou argumento de processo.

## Regras criadas pelo gateway

Para cada device `id`, o gateway cria o client DynSec com `username` e
`clientid` iguais a `id`, uma senha aleatória de 256 bits e a role
`device-<id>`. A role permite somente:

- publicar telemetria, estado, evento e resultado de comando nos respectivos
  tópicos canônicos `devices/<id>/...`;
- assinar e receber apenas `devices/<id>/command`, quando esse tópico existe.

O gateway não grava a senha no SQLite. A resposta de provisionamento continua
sendo a única oportunidade de levá-la ao firmware.

## Cutover e rollback

1. Valide o DynSec em `1884`, inclusive teste positivo e negativo de ACL.
2. Crie no DynSec a identidade do gateway, com direito de publicar comandos e
   assinar os tópicos ativos. Este passo é feito pelo procedimento de release,
   não pelo device onboarding.
3. Em uma janela curta, altere somente o endpoint MQTT de bootstrap do gateway
   para `1884`, reinicie o serviço uma vez e regrave cada firmware com a nova
   porta e sua credencial individual.
4. Confirme telemetria, comandos e reconexão de cada device. Só então pare o
   Mosquitto legado e arquive os arquivos de backup.

Enquanto o serviço legado permanecer ativo, rollback é possível: volte o
endpoint do gateway para `1883` e use os firmwares/credenciais anteriores.
Não apague `passwd` ou `acl` até o aceite completo.
