# Registry operacional em SQLite

> **Status: migracao em andamento.** O registry SQLite e a aplicacao dinamica
> da politica MQTT ja existem. O adaptador DynSec esta no gateway; a ativacao
> no Orange Pi e o cutover dos devices seguem `dynsec-migration.md`. O
> `cyd-monitor` ja recebe a configuracao MQTT no NVS durante o provisionamento;
> os demais firmwares seguem como proxima etapa.

## Objetivo

O gateway deve permanecer em execucao enquanto dispositivos sao cadastrados,
desabilitados, removidos ou alterados. As mudancas precisam sobreviver a quedas
e ser auditaveis, sem editar YAML, entrar por SSH ou reiniciar gateway e
Mosquitto.

Configuracao mutavel passa a ser dado tipado no banco; arquivos deixam de ser
a interface de administracao.

```text
gateway-web/API
       |
       v
control plane ----> registry SQLite <---- runtime manager do gateway
       |                    |                         |
       |                    v                         v
       +----------> reconciliador ----------> Mosquitto / MQTT
```

## Estado desta entrega

- O processo `run` abre `gateway.db`, importa `devices`/`routes` do YAML uma
  unica vez quando o registry esta vazio e, a partir dai, le o registry.
- Mudancas de revisao sao observadas pelo processo MQTT e aplicadas sem
  `systemctl restart`; novos topicos sao assinados antes de a nova politica ser
  exposta e topicos removidos deixam de ser aceitos imediatamente.
- A API de administracao usa o registry para cadastrar, habilitar, desabilitar
  e remover devices, alem de criar, listar e remover rotas; a listagem e a
  publicacao de comandos leem a politica MQTT atualmente aplicada, nao o YAML
  de bootstrap.
- Uma rota so pode ligar um topico inbound de device habilitado a um topico
  `command` de device habilitado. A remocao de um device remove no mesmo
  commit as rotas que o referenciam, sem deixar encaminhamentos pendentes.
- Quando `IOT_GATEWAY_DYNSEC_URL` esta configurada, a administracao usa a API
  DynSec do broker e nao edita arquivos nem recarrega servicos. O script legado
  fica somente como compatibilidade durante a migracao.

## Decisoes de escopo

- `gateway.yaml` deixa de ser a fonte de verdade de devices, topicos, rotas e
  politicas de encaminhamento. Ele sera removido do fluxo operacional apos a
  migracao.
- Senhas **nao** sao copiadas para o SQLite. O banco armazena identidade,
  estado e referencias logicas; o broker armazena autenticacao e autorizacao.
- O processo MQTT nao e reiniciado por alteracoes de cadastro. Ele aplica um
  snapshot validado de configuracao em memoria.
- Configuracao inicial do sistema continua existindo, mas e minima e imutavel
  no dia a dia: unidade systemd, bootstrap do Mosquitto e localizacao do
  diretorio de dados. Esses arquivos sao instalados uma vez, nao editados por
  device.
- A transicao do broker usa o Dynamic Security Plugin do Mosquitto. Clientes,
  roles e ACLs sao mudados por sua API de controle enquanto ele esta rodando.

## Fontes de verdade

| Dado | Dono | Persistencia | Nunca deve conter |
| --- | --- | --- | --- |
| Device, topicos, profile, rotas, politicas e estado desejado | Gateway | `gateway.db` | Senha MQTT em texto puro |
| Operacoes de provisionamento e auditoria | Gateway | `gateway.db` | Payload MQTT sensivel em logs |
| Hash de senha MQTT, client e ACL/role | Mosquitto | Dynamic Security | Copia no registry do gateway |
| Credencial estavel do gateway | Sistema operacional | credential/env protegido do systemd | YAML, flags ou logs |
| Wi-Fi e MQTT de cada ESP32 | Device | NVS (etapa futura) | Git ou resposta repetivel de API |

O arquivo interno que o plugin Mosquitto usa para persistir seu estado pode
continuar existindo. Ele e detalhe do broker, de posse do usuario `mosquitto`,
e nao um arquivo que operadores editam para administrar devices.

## Bootstrap minimo

O binario deve iniciar a partir de um diretorio de dados conhecido:

```text
/usr/local/bin/iot-gateway run --data-dir /var/lib/iot-gateway
```

O SQLite fica em `/var/lib/iot-gateway/gateway.db`. O systemd fornece apenas a
credencial que o gateway precisa para falar com o broker; uma variavel de
ambiente protegida e aceitavel inicialmente. Em etapa posterior ela pode usar
credentials nativas do systemd. Nem caminho nem valor de senha pertencem a
tabelas de devices.

Endereco do broker, listeners, limites da outbox e outras preferencias
operacionais mutaveis podem viver em `gateway_settings`. O caminho para abrir
o banco nao pode depender dessa mesma tabela: isso evita o problema de
bootstrap em que o processo precisa ler o banco antes de saber onde ele esta.

## Modelo de dados

O schema final sera definido por migrations SQL versionadas. A separacao abaixo
e uma fronteira de dominio; nao e instrucao para guardar JSON/YAML arbitrario
numa coluna.

```text
gateway_settings
  key, typed_value, revision, updated_at

devices
  id, type, profile, desired_state, active_state, revision,
  created_at, updated_at, last_error

device_topics
  device_id, kind, topic, direction

device_forwarding
  device_id, telemetry_to_vps, state_to_vps,
  events_to_vps, commands_from_vps

routes
  id, source_topic, destination_topic, transform_type,
  command_type, qos, retain, desired_state

provisioning_operations
  id, idempotency_key, device_id, kind, desired_revision,
  state, attempts, error, created_at, updated_at

config_revisions
  revision, actor, reason, created_at

audit_events
  id, actor, action, resource_type, resource_id, revision,
  outcome, created_at
```

`desired_state` expressa a intencao do operador (`active`, `disabled` ou
`deleted`); `active_state` registra o que de fato foi aplicado no runtime e no
broker. Essa diferenca impede que uma resposta HTTP declare sucesso quando uma
falha ou queda interrompeu a aplicacao.

As invariantes atuais continuam obrigatorias: IDs unicos, topico canonico
`devices/<device-id>/<tipo>`, dono unico por topico, direcoes permitidas e
profile conhecido. O banco tambem deve impor unicidades por indices, nao
apenas pela aplicacao.

## Componentes em runtime

### Control plane

A API de administracao deixa de escrever arquivos e chamar `systemctl`. Ela
valida o request, abre uma transacao curta no registry, cria uma
`provisioning_operation` idempotente e devolve seu estado. Uma chave de
idempotencia por request impede que um retry gere outra senha ou dois devices.

O control plane pode continuar separado do processo MQTT por motivo de
exposicao/autenticacao, mas operacoes comuns nao precisam de root. Root fica
restrito a instalar/atualizar servicos e configurar inicialmente o broker.

### Reconciliador

O reconciliador le operacoes pendentes e conduz cada uma a um estado terminal
ou a nova tentativa com backoff. Ele e o unico adaptador que conversa com o
Dynamic Security para criar/remover client, senha e role/ACL.

SQLite e Mosquitto nao compartilham transacao ACID. A garantia deve ser
convergencia idempotente, nao uma falsa atomicidade:

1. Persistir intencao e operacao no SQLite.
2. Aplicar ou consultar o estado desejado no Mosquitto.
3. Marcar a operacao aplicada e publicar nova revisao do registry.
4. Repetir com seguranca depois de crash, timeout ou resposta perdida.

Uma operacao nunca fica silenciosamente esquecida: `attempts`, `last_error` e
idade devem aparecer no status, metricas e UI.

### Runtime manager

O processo MQTT acompanha a revisao ativa do registry e monta um snapshot
imutavel contendo devices, topicos, politicas, rotas e subscriptions desejadas.
O callback MQTT consulta esse snapshot por `atomic.Pointer` (ou equivalente),
nunca mapas alterados concorrentemente.

Para adicionar um device, o manager valida o snapshot completo, prepara as
subscriptions, instala o snapshot que aceita mensagens/comandos e registra a
revisao aplicada. Para desabilitar/remover, primeiro instala uma politica que
rejeita trafego e comandos; depois desassina topicos e o reconciliador revoga a
credencial. Essa ordem favorece seguranca durante falhas.

Em toda reconexao MQTT, o manager reconcilia a lista completa de subscriptions
desejadas. Isso cobre devices criados enquanto o broker estava indisponivel e
nao depende de detalhes de sessao persistente da biblioteca MQTT.

## Fluxos operacionais

### Provisionar

```text
API -> registry: device + operacao "provision" (pending)
reconciliador -> Mosquitto: client, senha de uso unico, role/ACL minima
reconciliador -> registry: operacao aplicada, device desired=active
runtime manager: aplica revisao e subscriptions sem restart
API/UI: mostra senha somente nesta resposta segura
```

A senha nunca e registrada em `audit_events`, logs ou leitura posterior de
`ListDevices`. Se a conexao do cliente cair apos a geracao, a operacao exige
recuperacao segura pelo operador, em vez de rotacionar automaticamente uma
senha que o device ja pode ter recebido.

### Habilitar, desabilitar e remover

Habilitar/desabilitar muda somente a revisao desejada. O runtime aplica a
politica em memoria e confirma `active_state`; desabilitar nao exige nova senha.

Para remover, o runtime para de aceitar/publicar para o device, o
reconciliador revoga sua identidade no broker e so entao a operacao termina. A
linha pode permanecer como `deleted` por periodo configuravel para auditoria;
a limpeza fisica posterior nao e condicao para seguranca.

## Credenciais dos devices e NVS

NVS (*non-volatile storage*) e a area de armazenamento persistente do ESP32.
Na fase atual, SSID, senha Wi-Fi e senha MQTT podem continuar compilados no
`secrets.h` local. A evolucao planejada e um modo de provisionamento que recebe
essas credenciais uma vez e as grava em NVS.

Isso reduz arquivos locais por device e permite reprovisionar sem reflash, mas
e etapa separada: antes e preciso definir autenticacao do provisionamento,
reset seguro, criptografia quando disponivel e o que fazer se o hardware for
roubado ou reutilizado. NVS nao torna segredo magicamente seguro; ele deixa de
ser configuracao de build e nao deve entrar no Git.

## Resiliencia e observabilidade

- Callbacks MQTT nao devem executar trabalho lento sem limite. Uma fila interna
  limitada separa recepcao MQTT de SQLite, uplink e rotas; saturacao precisa ter
  politica explicita e mensuravel.
- A outbox continua limitada por quantidade, bytes e idade. O mesmo principio
  vale para operacoes pendentes, auditoria e filas em memoria.
- Status deve incluir revisao desejada/aplicada, conectividade MQTT, devices
  por estado, subscriptions desejadas/ativas, operacoes pendentes e erro mais
  recente, sem segredo ou payload.
- Reinicio pelo systemd continua sendo recuperacao de falha do processo, nao
  mecanismo para aplicar configuracao.
- Migrations, backup consistente e restauracao do SQLite entram no procedimento
  de recuperacao do gateway.

## Plano de migracao

1. **Preparar registry:** criar migrations, repositorio tipado e importador
   somente-leitura de `gateway.yaml`; nenhum caminho novo escreve YAML.
2. **Leitura dinamica:** introduzir runtime manager e aplicar SQLite sem
   restart, inicialmente com o Mosquitto atual.
3. **Operacoes recuperaveis:** adicionar revisoes, idempotencia,
   `provisioning_operations`, reconciliacao e observabilidade.
4. **Migrar broker:** habilitar Dynamic Security, importar identities/ACLs e
   validar testes positivos e negativos de permissao.
5. **Virar fonte de verdade:** API grava somente no registry; YAML fica apenas
   como artefato de backup/importacao por uma versao.
6. **Remover legado:** retirar parser/escritor YAML de devices, script de ACL
   por arquivo e restarts disparados pela administracao.
7. **Provisionamento ESP32:** desenhar e implementar NVS posteriormente, sem
   bloquear a migracao do gateway.

Durante a migracao nao pode haver dois escritores. Em cada etapa, uma unica
fonte de verdade e mutavel; a outra apenas e importada, exportada ou lida para
compatibilidade. Isso evita divergencia entre YAML, SQLite e broker.

## Criterios de aceite

- Cadastrar, habilitar, desabilitar e remover nao executa `systemctl restart`
  nem exige editar arquivo sob `/etc`.
- Queda entre etapas de provisionamento e recuperada por reconciliacao, sem
  credencial orfa invisivel ou device ativo sem politica.
- Reiniciar o gateway recupera a mesma revisao pelo SQLite e reconstroi as
  subscriptions necessarias.
- Repetir request administrativo e seguro e nao rotaciona senha.
- Senhas nao aparecem em YAML, SQLite, logs, auditoria, argumentos de processo
  ou respostas posteriores ao provisionamento.
- A suite cobre diffs em runtime, reconexao MQTT, falhas entre SQLite e broker
  e recuperacao apos queda.
