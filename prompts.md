
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















# Task: document exactly how Grafana plugins get into our Grafana pods

## Context
We are adding a new custom Grafana **backend datasource plugin**:
- Plugin ID: `kotak-starrocks-datasource`.
- Zip layout: a top-level folder `kotak-starrocks-datasource/` containing `plugin.json`, `module.js`, `img/` and a Linux binary named like `gpx_kotak_starrocks_linux_amd64`.
- Signing is not decided yet: either a private signature or `allow_loading_unsigned_plugins`.

Before anyone touches config, I need a precise map of how plugins are delivered today.

This is research only:
- Do NOT edit, commit or push anything.
- Do NOT print secrets, tokens, passwords, account IDs or full ARNs. Mask them, e.g. `arn:aws:iam::****:role/...`.

## Repo and branches to check
- Repo: obser-aws-deploy-config, focus on uat env

## Questions to answer (give file path + line numbers + a short quoted snippet for each)

### 1. Plugin delivery pipeline
1. Find `grafana-plugin-upload.yml`, or whatever uploads plugin zips. What triggers it? Which S3 bucket and prefix does it write to, with the bucket name masked if sensitive? How are plugin versions named?
2. List every plugin currently delivered this way: plugin ID, version and zip name. Which of them have backend binaries?
3. Is any plugin installed a different way, e.g. `GF_INSTALL_PLUGINS`, the Helm `plugins:` list, a baked-in image, or a sidecar? Say which and where.

### 2. How the pod picks plugins up
4. Show the init container(s) in the Grafana Helm values for EACH environment, including:
   - image, command and args;
   - the exact `aws s3 cp` / sync and unzip commands;
   - the target directory;
   - IAM / IRSA / service account used for S3 access.
5. Which volume is mounted at the plugins path (emptyDir, PVC, other)? What is the mount path in the init container and in the Grafana container?
6. What is `paths.plugins` / `GF_PATHS_PLUGINS` set to? Confirm it matches the directory the init container writes to.
7. Is there a `chmod +x` (or similar) step for backend binaries (`gpx_*`)? In which environments is it present or missing?
8. Is there any plugin allowlist or signature setting: `allow_loading_unsigned_plugins`, `plugin_admin_enabled`, `plugins.preinstall`, `plugin_catalog_url`, `GF_PLUGINS_*` env vars? Give the per-env values.

### 3. Grafana deployment specifics
9. Helm chart name and version, and the workload kind (StatefulSet/Deployment) and replica count per env.
10. Grafana image and tag per env. Is it the official image or a wrapper? If a wrapper, where is its Dockerfile, and does it add anything plugin-related?
11. How changes roll out: ArgoCD / Flux / CI `helm upgrade` / manual. Which file or app definition controls that? Does a values change restart the pod automatically?
12. Container security context: `readOnlyRootFilesystem`, `runAsUser`, `fsGroup`, any `noexec` mounts. Anything that would stop a plugin binary from executing.
13. Egress or network policies affecting the Grafana pod. Can it reach the StarRocks FE on port 9030?

### 4. Datasource provisioning
14. Where are datasources provisioned for each env and org? Give the file paths, and say how they get into the pod (ConfigMap, sidecar, values `datasources:` block).
15. Show one existing StarRocks (`type: mysql`) datasource entry in full, with secrets masked, and one entry that already uses `oauthPassThru: true`.
16. How are secret files referenced? Confirm the `$__file{...}` usage and the secrets CSI mount, e.g. `/mnt/grafana-secrets/domain-kotak-cacerts`: SecretProviderClass name, mount path, and which env vars or files use it.

### 5. Auth settings that affect token forwarding
17. `auth.generic_oauth` per env: `use_refresh_token`, `scopes`, `login_attribute_path`, `auth_url` (does it include `audience=`?), and `token_url`. Mask `client_id` (show the first 4 characters only) and never show `client_secret`.
18. Are feature toggles set (`[feature_toggles]` / `GF_FEATURE_TOGGLES_ENABLE`), especially `improvedExternalSessionHandling` or `idForwarding`?

## Output format
Write ONE markdown file: `grafana-plugin-delivery-findings.md` in the repo root. Do not commit it.
1. A short "How plugins reach Grafana today" flow, as a single line or small ASCII diagram.
2. One section per question number above. Each gets the answer, then `path:line`, then a quoted snippet of at most 10 lines.
3. A table of differences between environments (dev / uat / prod) for: init container, chmod, plugins path, unsigned-plugin setting, rollout method.
4. A list called "Exact changes needed to add `kotak-starrocks-datasource`". For each change give the file path, what to add, and in which env. Describe the changes only; do not make them.
5. A list called "Unknowns / couldn't find". State what's missing rather than guessing. 
