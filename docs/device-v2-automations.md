# API de automações da Device Platform V2

A `DeviceV2API`, protegida pela autenticação da borda, expõe estes métodos JSON
via POST em `/iot.gateway.api.v2.DevicePlatformService/<método>`. O registry
valida os campos da regra contra os manifests dos devices de origem e destino.
O servidor define `updatedAt` em cada criação, edição e alteração de estado.

| Método | Requisição | Resposta |
| --- | --- | --- |
| `ListAutomationRules` | `{}` | `{ "rules": [AutomationRuleV2] }` |
| `CreateAutomationRule` | `{ "rule": AutomationRuleV2 sem updatedAt }` | `{ "rule": AutomationRuleV2 }` |
| `UpdateAutomationRule` | `{ "rule": AutomationRuleV2 sem updatedAt }` | `{ "rule": AutomationRuleV2 }` |
| `SetAutomationRuleEnabled` | `{ "ruleId": string, "enabled": boolean }` | `{ "rule": AutomationRuleV2 }` |
| `RemoveAutomationRule` | `{ "ruleId": string }` | `{}` |

Uma edição preserva o ID, substitui os campos editáveis e valida novamente o
canal, evento, condição, comando e parâmetros. Habilitar ou desabilitar altera
somente `enabled`, sem sobrescrever outros campos editados em paralelo. Remover
define `deleted_at_ns`; a listagem e a execução MQTT ignoram a regra removida.
A linha permanece no SQLite para preservar as chaves estrangeiras e o histórico
de execução e deduplicação, por isso IDs removidos não podem ser reutilizados.

Editar, alterar o estado e remover retornam HTTP 404 quando a regra está ausente
ou removida. Criar uma regra com ID já usado retorna HTTP 409; conteúdo inválido
retorna HTTP 400.
