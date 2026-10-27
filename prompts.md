
┌──────────────────────────────────────────────────────────────────────────────┬─────────────────────────────────────────────────────────────────────┬────────────────────┐
│                                  Info area                                   │                        Likely repo / branch                         │    Question IDs    │
├──────────────────────────────────────────────────────────────────────────────┼─────────────────────────────────────────────────────────────────────┼────────────────────┤
│ A client already logging in to StarRocks with a JWT (Logging UI or POC code) │ Logging UI repo, POC branch                                         │ P1 (most valuable) │
├──────────────────────────────────────────────────────────────────────────────┼─────────────────────────────────────────────────────────────────────┼────────────────────┤
│ StarRocks fe.conf, JWT setup, users                                          │ Wherever StarRocks is deployed, plus the POC branch                 │ S1–S7              │
├──────────────────────────────────────────────────────────────────────────────┼─────────────────────────────────────────────────────────────────────┼────────────────────┤
│ Load balancer in front of port 9030, network rules                           │ obser-aws-infra                                                     │ S2, S7             │
├──────────────────────────────────────────────────────────────────────────────┼─────────────────────────────────────────────────────────────────────┼────────────────────┤
│ Ranger setup                                                                 │ Ranger config repo                                                  │ R1                 │
├──────────────────────────────────────────────────────────────────────────────┼─────────────────────────────────────────────────────────────────────┼────────────────────┤
│ MCP server and its auth proxy                                                │ obser-aws-deploy-config (grafana-mcp-deploy.yml), the MCP auth repo │ M1–M4              │
├──────────────────────────────────────────────────────────────────────────────┼─────────────────────────────────────────────────────────────────────┼────────────────────┤
│ Grafana alert rules, public dashboards                                       │ obser-aws-deploy-config                                             │ G1, G2             │
├──────────────────────────────────────────────────────────────────────────────┼─────────────────────────────────────────────────────────────────────┼────────────────────┤
│ Token facts                                                                  │ IDAM docs repo                                                      │ I1–I9              │
└──────────────────────────────────────────────────────────────────────────────┴─────────────────────────────────────────────────────────────────────┴────────────────────┘

P1 matters most. A client that already logs in with a JWT shows whether it had to force the cleartext login method. If it did, StarRocks probably won't request it on its own, and the Go driver will need a patch.

Prompt 1: deploy-config agent

Read-only research task. Do not modify any file except the single output file named below.

CONTEXT
We are building a custom Grafana backend datasource plugin (a copy of the built-in MySQL plugin
with a new plugin ID). It forwards the logged-in user's IDAM JWT to StarRocks 4.0.10 as a
cleartext MySQL password (mysql_clear_password). StarRocks validates the JWT and Apache Ranger
authorizes the user. docsskip/grafana-oss-deployment-context.md already covers Grafana Helm
values, plugin delivery and datasource provisioning, so do not repeat any of that.

REPOS AND BRANCHES YOU MAY READ (search all of them)
- <this repo> @ <branch>
- <path to other repo> @ <branch>
- <add more>

OUTPUT
Write ONE file: docsskip/starrocks-jwt-grafana-findings.md
It will be photographed and read from the photos, so:
- At most ~180 lines. Prefer tables to prose. Keep lines under ~110 characters.
- Copy exact strings (hosts, ports, config keys, claim names, URLs) into backticks.
- Give every fact a source: repo@branch:path:line.
- Keep the question IDs below. If you can't find something, write NOT FOUND and say where you searched.
- Mark any guess with "INFERRED:". Don't present a guess as a fact.
- Code snippets only for P1 and S4, at most 15 lines each.
- Mask secrets: no passwords, client secrets, tokens or private keys.
  Show AWS account IDs as ****1234, like the existing doc does.
- Start the file with "TOP ANSWERS": one line each for P1, S4, S3, M3 and G1.

QUESTIONS (highest priority first)

P1. Does any client already log in to StarRocks with an IDAM JWT (Logging UI, scripts, tests,
    Flink, POC code)? For each one give:
    - language, and the driver or library with its version
    - connection string or options, especially anything that forces the cleartext login method
      (defaultAuthenticationPlugin, --enable-cleartext-plugin, --default-auth, allowCleartextPasswords)
    - which token it sends (access token or ID token)
    - what it uses as the MySQL username
    - how it pools connections
    Show the connection code, secrets masked.
S4. StarRocks JWT setup: any CREATE SECURITY INTEGRATION or
    CREATE USER ... IDENTIFIED WITH authentication_jwt statements, scripts or docs, per env.
    List every property exactly: jwks_url, principal_field, required_issuer, required_audience,
    group_provider, permitted_groups, and any others.
S3. fe.conf or FE ConfigMap values per env: qe_max_connection, authentication_chain,
    access_control, all ranger_* keys, all ssl_* keys, any jwt or oauth keys. Use a key = value table.
M3. Does the MCP server reach StarRocks through Grafana (Grafana API /api/ds/query using a
    datasource), or does it connect to StarRocks directly? Give the evidence.
M1. How does the MCP server authenticate to Grafana: service account token, the user's IDAM JWT,
    auth proxy headers, or something else? Give the config keys and env var names (values masked).
M2. What does the MCP auth proxy do? Does it validate the IDAM JWT? Does it pass the JWT or the
    user's identity onward? To which component, and in which header?
M4. Is [auth.jwt] or [auth.proxy] configured in Grafana in any env?
G1. Are there alert rules or recording rules (provisioning files, Terraform, scripts) that query
    a StarRocks-backed type: mysql datasource? List env, org, rule title and datasource uid.
    Say so if alerting is only managed in the Grafana UI.
G2. Any [public_dashboards] or [snapshots] settings, or related feature toggles?
S1. StarRocks topology per env: how it's deployed (operator, Helm, EC2, other), how many FE nodes
    (leader / follower / observer), how many BE/CN nodes, and the image tag (confirm 4.0.10).
S2. What sits in front of FE port 9030 for `*.observability.logs.kotak.internal` and
    `fe-all-starrocks-prod.rds.kotak.internal`: NLB, K8s Service, Kong TCP, or other?
    What are the idle timeout settings? Check the obser-aws-infra Terraform too.
S5. max_user_connections for the logsro-* users, or the default that applies.
S6. TLS on the FE MySQL port: which certificate, issued by which CA (Kotak internal CA?),
    CN and SANs. Do they match the hostnames in S2?
S7. Can the FE nodes reach the IDAM JWKS URL? Check network policies, security groups,
    JVM proxy settings (-Dhttps.proxyHost etc.) and NO_PROXY.
R1. Ranger: where is it deployed and configured? What is the StarRocks service name in Ranger?
    Which JWT claim value becomes the Ranger username? What is the usersync source? How are groups
    and roles mapped? Show one example policy for a logs table, masked.

Prompt 2: IDAM docs agent

Read-only research task in this IDAM documentation repo. Do not modify any file except the
single output file named below.

CONTEXT
Grafana logs users in through IDAM as a Generic OAuth client:
- scopes "openid email phone profile offline offline_access"
- auth_style InParams (client credentials sent in the POST body)
- refresh tokens enabled
- prod endpoints: auth_url https://api.idam.kotak.internal/oauth2/auth,
  token_url .../oauth2/token, userinfo .../oidc/userinfo
- UAT host: uat.api.idam.kotak.internal
Grafana will forward the logged-in user's IDAM token to StarRocks 4.0.10. StarRocks validates
it as a JWT (jwks_url, principal_field, required_issuer, required_audience) and Apache Ranger
authorizes the user. MCP clients will do the same. We need exact token facts to configure this.

OUTPUT
Write ONE file: idam-jwt-facts.md at the repo root. Do not commit it.
It will be photographed and read from the photos, so:
- At most ~150 lines. Prefer tables to prose. Keep lines under ~110 characters.
- Copy exact strings (issuer, URLs, claim names, aud values) into backticks.
- Give every fact a source: path:line, or the doc title and section.
- Keep the question IDs below. If you can't find something, write NOT FOUND and say where you searched.
- Mark any guess with "INFERRED:".
- Never paste a real token. Show claims only, with values masked but the format kept,
  e.g. sub: "KMBL3xxxxx", email: "f***@kotak.com".
- Start the file with "TOP ANSWERS": one line each for I1 (issuer + JWKS URL per env),
  I2 (aud, sub, username claim), I4 (access token lifetime) and I6 (downstream forwarding rule).

QUESTIONS

I1. OIDC endpoints per env (UAT, prod, DR if any): discovery URL, exact issuer string, jwks_uri,
    and the introspection, revocation and end_session endpoints. Signing algorithm(s), and how
    often signing keys rotate (kid handling).
I2. Access token:
    - Is it always a JWT?
    - Full claim list with the meaning of each claim.
    - Exact aud value(s) for a confidential web client like Grafana: the client_id, a resource,
      or a list?
    - Format of sub.
    - Which claim holds the username, employee ID, email or UPN.
    - Names and format of the roles or groups claims.
    - The scope and azp/client_id claims.
    - Typical encoded size in bytes.
I3. ID token: how its claims differ from the access token's, and its exact aud.
I4. Lifetimes of the access token, ID token and refresh token (rotation? reuse detection?), and the
    maximum session length. Does a refresh_token grant return a new id_token?
I5. Does logging out or revoking a session invalidate access tokens that were already issued,
    or is introspection the only way to check?
I6. Forwarding tokens to downstream services: is one app (Grafana) allowed to send a user's access
    token to another service (StarRocks, MCP)? Are there audience rules? Does IDAM support token
    exchange (RFC 8693), resource indicators (RFC 8707) or per-audience tokens? Is there a
    documented pattern for this?
I7. "Hybrid IDAM model": what does it mean? How do roles, and the upcoming template-based
    policies, appear in tokens?
I8. Service-to-service: is the client credentials grant supported? What do sub and the other
    claims look like in machine tokens? (We need this for an alerting / MCP fallback.)
I9. Any docs on onboarding StarRocks, Ranger, Grafana or the Observability platform to JWT auth.
