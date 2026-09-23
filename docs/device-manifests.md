# Manifests de dispositivos e provisionamento genérico

> **Status: proposta aprovada; implementação ainda não iniciada.**
>
> Este documento é o checklist de acompanhamento para substituir templates,
> profiles e RPCs específicos de hardware por um control plane declarativo.

## Problema

Hoje, suportar uma nova família de dispositivo — ou evoluir uma existente —
pode exigir alterações coordenadas no firmware, no `iot-gateway` e no
`gateway-web`. Exemplos atuais são templates compilados, validação de comandos
compilada e operações distintas de provisionamento para CYD e LED.

Isso impede que uma capability como `set_brightness` ou `blink` seja adotada
apenas com firmware novo e uma atualização declarativa de política.

## Decisão

O gateway manterá um **catálogo versionado de manifests** no SQLite. Um
manifest é JSON textual editável no Gateway Web; não é um arquivo runtime no
Orange Pi. Arquivos JSON poderão existir somente como exportação, backup ou
importação explícita.

O manifest descreve, de forma limitada e validada:

- identidade e compatibilidade de provisionamento (`model`, versão de
  protocolo);
- tópicos MQTT que a identidade DynSec poderá usar;
- comandos aceitos e schema de seus parâmetros;
- eventos emitidos e schema de seus payloads;
- metadados de apresentação para o Web.

Ele **nunca** contém senha, host MQTT de instalação, shell script, ACL Mosquitto
bruta, JavaScript, URL arbitrária ou código executável.

```text
Editor textual no Gateway Web
             |
             v
  validação estrutural e semântica no gateway
             |
             v
manifest publicado + revisão no SQLite
             |
             +----> provisionamento por IP
             +----> validação de comandos/eventos
             +----> futuro editor de automações
```

## Contrato de provisionamento comum

Todo firmware que suporte cadastro por IP implementará o mesmo contrato LAN:

```text
GET  /v1/device-info
POST /v1/provision
```

`GET /v1/device-info` deverá informar, no mínimo:

```json
{
  "model": "esp32c3-led",
  "protocol_version": 1,
  "device_uid": "identidade estável de hardware",
  "firmware_version": "1.2.0"
}
```

`POST /v1/provision` recebe o envelope MQTT comum: `device_id`, host, porta,
usuário e senha. O firmware o persiste no NVS e fecha a interface temporária
de provisionamento. A senha flui somente do processo admin para o device; não
é devolvida pela API, registrada em log nem salva no SQLite.

O gateway oferecerá uma operação única conceitual:

```text
ProvisionDeviceByIP(device_id, manifest_id, device_ip)
```

Ela valida o IP privado, consulta o endpoint, confere `model` e versão contra
o manifest, cria a identidade DynSec inicialmente desabilitada, entrega a
configuração ao NVS e só então habilita identidade e device no registry.

## Exemplo de manifest

```json
{
  "schema_version": 1,
  "id": "esp32-c3-led",
  "display_name": "ESP32-C3 LED",
  "provisioning": {
    "protocol": "http-nvs-v1",
    "model": "esp32c3-led",
    "required_protocol_version": 1
  },
  "mqtt": {
    "topics": ["state", "command"]
  },
  "capabilities": {
    "commands": [
      {
        "type": "set_led",
        "parameters": {
          "on": { "type": "boolean", "required": true }
        }
      }
    ],
    "events": [
      {
        "type": "led.state_changed",
        "payload": {
          "on": { "type": "boolean", "required": true }
        }
      }
    ]
  }
}
```

Quando o firmware futuramente implementar `blink`, uma nova revisão poderá
declarar seu comando. O Web renderizará a capability a partir do manifest e o
gateway validará os parâmetros sem ganhar código específico de LED. O firmware
ainda precisa ser atualizado, pois ele é quem executa a ação física.

## Revisões e segurança operacional

- Manifests possuem estados `draft`, `published` e `archived`.
- Somente uma revisão publicada pode provisionar novos devices.
- Cada device registra `manifest_id` e `manifest_revision` usados no cadastro.
- Editar um manifest cria uma revisão; nunca altera silenciosamente policy ou
  capabilities de devices existentes.
- Uma futura migração de revisão será uma operação explícita, auditada e
  reversível enquanto ainda não entregar nova configuração ao device.
- O gateway valida IDs, tópicos, schemas, limites de tamanho e compatibilidade
  antes de publicar uma revisão.
- A autenticação da API e a autorização de editar/publicar manifests seguem no
  processo admin. Pairing de uso único para o endpoint LAN é evolução posterior.

## Relação com automações futuras

Hooks não fazem parte da primeira entrega, mas o manifest já catalogará eventos
e comandos para que eles possam ser ligados de modo seguro:

```text
evento de led1 -> condição declarativa -> comando de led2
```

O motor futuro deve persistir eventos, deduplicar por `event_id`, criar ações
na outbox e impedir ciclos por `causation_id`/limite de saltos. Dispositivos
nunca receberão credenciais ou endereços uns dos outros.

## Checklist de acompanhamento

### Fundamentos já disponíveis

- [x] Registry SQLite com atualização runtime sem reiniciar o processo MQTT.
- [x] DynSec para identidade, role e ACL de device sem editar `passwd`/`acl`.
- [x] Provisionamento NVS por IP para CYD e ESP32-C3 LED.
- [x] Senha MQTT entregue ao device sem retorno para o navegador.
- [x] Editor visual de rotas locais persistidas no SQLite.

### Marco 1 — modelo de manifest no SQLite

- [x] Adicionar migrations para `device_manifests`, revisões e vínculo da
  instância à revisão provisionada.
- [x] Definir schema JSON versionado e validação Go sem executar conteúdo do
  manifest.
- [x] Criar seeds de `esp32-c3-led`, `cyd-monitor` e `orangepi-monitor`.
- [x] Expor leitura/listagem de manifests publicados pela API admin.
- [x] Persistir auditoria de seeds com autor, horário, revisão e conteúdo.
- [ ] Registrar auditoria de autoria e revisão anterior nas futuras operações
  de rascunho/publicação (Marco 3).

### Marco 2 — provisionamento genérico

- [ ] Tornar `protocol_version`, `device_uid` e `firmware_version` obrigatórios
  no contrato de primeiro boot dos firmwares.
- [ ] Generalizar o cliente HTTP de provisionamento por protocolo, sem aceitar
  modelo inesperado.
- [ ] Implementar `ProvisionDeviceByIP` usando manifest publicado.
- [ ] Garantir rollback de DynSec/registry antes da entrega ao NVS e estado de
  recuperação explícito depois dela.
- [ ] Migrar CYD e LED para a RPC genérica; manter RPCs legadas apenas durante
  uma janela de compatibilidade definida.

### Marco 3 — Web textual mínimo

- [ ] Criar página de lista de manifests, com status e revisão publicada.
- [ ] Criar editor JSON textual com validação remota e mensagens por campo.
- [ ] Permitir criar rascunho, duplicar, validar, publicar e arquivar revisão.
- [ ] Alterar “Novo dispositivo” para selecionar um manifest e informar ID/IP.
- [ ] Exibir manifesto e revisão usados em cada dispositivo cadastrado.

### Marco 4 — comandos declarativos

- [ ] Ler comandos e schemas do manifest no runtime em vez de profiles Go
  compilados.
- [ ] Renderizar formulário de comando genérico no Gateway Web.
- [ ] Validar parâmetros no gateway antes de publicar MQTT.
- [ ] Migrar `led.v1` e remover templates/profiles específicos quando não
  houver devices dependentes da compatibilidade legada.

### Marco 5 — base para automações

- [ ] Padronizar eventos transitórios separados de state retained.
- [ ] Persistir `event_id`, `causation_id` e resultado de comando.
- [ ] Modelar regras evento → condição → ação no SQLite.
- [ ] Executar ações via outbox com deduplicação e prevenção de ciclos.
- [ ] Só então construir a UI dedicada de automações.

## Critério de conclusão da primeira etapa

Um operador consegue cadastrar uma nova família compatível sem recompilar
`iot-gateway` ou `gateway-web`: publica um manifest textual válido, grava o
firmware que implementa o protocolo comum e provisiona o IP pelo Web. A única
configuração compilada no ESP continua sendo a conectividade Wi-Fi inicial.
