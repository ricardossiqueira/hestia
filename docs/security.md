# Segurança

## Limites iniciais

- Mosquitto ficará acessível apenas na LAN confiável; não será exposto diretamente à internet.
- Cada ESP32 terá credenciais MQTT próprias assim que o primeiro teste simples estiver funcionando.
- ACLs do Mosquitto deverão limitar cada ESP32 aos seus próprios tópicos `devices/<id>/#`.
- O gateway terá uma identidade MQTT própria, com acesso de leitura/escrita aos tópicos declarados.
- O gateway só aceita comandos externos pelo uplink autenticado futuro e somente quando `commands_from_vps: true`.

## Segredos

- Não versionar senhas, certificados, chaves privadas ou arquivos `.env` reais.
- Usar permissões restritivas nos arquivos de configuração locais.
- As chaves WireGuard futuras pertencem ao sistema operacional e não ao repositório.

## A fazer antes de expor a VPS

- TLS ou túnel autenticado validado de ponta a ponta.
- Autenticação mútua gateway–VPS.
- Auditoria de comandos recebidos e publicados.
- Estratégia de rotação de credenciais.
