# Contas de operador e sessão da Hera

O módulo `internal/operatorauth` guarda contas e sessões no mesmo arquivo SQLite
do Hestia, em tabelas próprias. A API pública continua sendo a borda de
autenticação; `iot-gateway run` não recebe senhas nem cookies.

`GET /auth/session` informa se existe uma conta inicial e restaura uma sessão
válida. Quando não há conta, a Hera oferece o cadastro inicial. Esse único
cadastro usa `POST /auth/register` com a credencial HTTP Basic admin atual no
header `Authorization` e o novo `username`/`password` no JSON. O registro só é
aceito enquanto não existe conta. Não há cadastro público nem senha padrão.

`POST /auth/login` verifica a senha do operador, guardada como hash bcrypt, e
emite um token aleatório em cookie `HttpOnly`, `Secure` e `SameSite=Strict`.
Somente o hash do token fica no SQLite. A sessão dura 30 dias, inclusive após
reinício do processo ou fechamento do navegador. `POST /auth/logout` revoga
essa sessão. A API exige `X-CSRF-Token` nas chamadas autenticadas por cookie;
o token é devolvido por login e `GET /auth/session` e fica só na memória da
Hera. O login limita tentativas falhas por origem de rede.

As rotas Connect e Device Platform aceitam a sessão. A credencial Basic admin
continua aceita para clientes existentes durante a migração; Hera só a usa uma
vez no cadastro inicial. Esse caminho legado deve ser removido ou restringido
quando todos os clientes tiverem migrado.

O cookie seguro exige HTTPS no navegador. A implantação atual documentada em
`deploy/README.md` usa HTTP direto na LAN, portanto precisa de uma borda HTTPS
que sirva Hera e encaminhe `/api/` ao Hestia sob a mesma origem. Configure os
dois endereços públicos da Hera como `/api` e não exponha a porta HTTP do
Hestia a clientes fora dessa borda. Apenas adicionar o cookie ao HTTP atual
não entrega um login persistente seguro.
