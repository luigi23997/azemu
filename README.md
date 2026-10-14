# azemu

Run your Azure Function Apps and App Services locally, wired like Azure: same app settings, same hostnames, managed identity, Easy Auth headers, Storage (Azurite) and Key Vault references. No code changes.

`terraform plan` is the infra contract (app settings, storage accounts, Easy Auth), `azemu.yaml` maps each code folder to its app, and every app is rebuilt into an image on change.

**Status: early (v0.1).** Expect rough edges; see [docs/PLAN.md](docs/PLAN.md) for the design and roadmap. Licensed under [Apache 2.0](LICENSE).

## Install

```
go install github.com/luigi23997/azemu/cmd/azemu@latest
```

The proxy runs from an image built from this repo's `Dockerfile`:

```
docker build -t azemu/proxy:dev .
```

## Prerequisites

Docker (with compose >= 2.24 and buildx), terraform and `az login` (only to produce the plan; see limits). Check with:

```
azemu doctor
```

### Windows

Native Windows (PowerShell or cmd) needs Docker Desktop in Linux-containers mode (WSL2 backend, the default). Prerequisites in one go:

```
winget install Docker.DockerDesktop GoLang.Go Hashicorp.Terraform Microsoft.AzureCLI Git.Git
```

Then install as above from a new terminal; `azemu.exe` lands in `%USERPROFILE%\go\bin`. Running inside WSL2 also works: there it is plain Linux, keep the project in the Linux filesystem (`~/...`), not `/mnt/c`.

- Keep app code in LF: a CRLF shell script copied into a Linux image fails with `\r: not found`. Add `* text=auto eol=lf` to the project's `.gitattributes`.
- File permissions are not enforced: secrets under `.azemu/` are readable by your Windows user, like any other file in your profile.

### Without Docker (`--native`)

For machines that cannot run containers, such as an Azure Virtual Desktop without nested virtualization: `azemu up --native` runs everything as plain processes, no admin rights needed.

```
npm i -g azure-functions-core-tools@4    # needs Node.js 22+; plus the Python version of your apps
azemu doctor --native
azemu trust                              # once: Windows asks to confirm adding the azemu CA to your user's roots
azemu up --native
```

- The proxy runs inside azemu and every app gets `HTTPS_PROXY` pointing at it: emulated hostnames (`*.azurewebsites.net`, the storage accounts, Graph) are answered locally, everything else is forwarded to the real host (through your own `HTTPS_PROXY`, if you have one). Ingress ports, Easy Auth headers and managed identity work as with Docker.
- Azurite runs through `npx` (downloaded on first run), data in `.azemu/azurite`.
- Zip apps run with `func start`: Python apps get a virtualenv in `.azemu/venv/<app>` (`pip install -r requirements.txt`, or `uv export` for a `uv.lock` project), Node apps get `npm ci` and `npm run build`. Docker apps need the command that starts them, listening on `{port}` (also in `$PORT`):

  ```yaml
  apps:
    web-backend:
      kind: docker
      run: "python -m uvicorn main:app --host 127.0.0.1 --port {port}"
  ```

  `run:` also overrides `func start` for a zip app.
- A change restarts the app; Ctrl-C stops everything (`status`, `logs` and `down` are for the Docker stack).
- `azemu trust` is needed on Windows and macOS because the Functions host (.NET) only trusts the OS store there. It trusts a CA whose key is in `~/.azemu/ca`: anything that can read that file can mint certificates your user's programs accept.

Less faithful than Docker: apps run on your machine's Python/Node and OS, not Azure's Linux images; seed steps are skipped; a client that ignores `HTTPS_PROXY` (Node's `fetch` before Node 24, raw sockets) talks to the real Azure host and fails there, since it carries emulator keys and tokens.

## Quickstart

```
azemu init --env dev       # discover apps from the pipelines, write azemu.yaml; review it
azemu secrets init         # add the Key Vault secret names the apps reference to .env; fill in the values
azemu up                   # plan, build, start everything in parallel, then watch and rebuild
```

Other commands: `azemu status`, `azemu env <app>` (resolved settings, secrets masked), `azemu logs [app]`, `azemu seed`, `azemu down [--volumes]`.
`azemu up --no-watch` starts and exits; `azemu up func-orders web-backend` starts a subset; `--as you@example.com` sets the Easy Auth identity.

Each app is served on `http://127.0.0.1:<port>` (ports in `.azemu/ports.json`, stable across runs), and inside the network on its real `https://<name>.azurewebsites.net`.

## Logs dashboard

`azemu up` also starts [Dozzle](https://dozzle.dev) (read-only, no start/stop/shell actions) and prints `Logs dashboard: http://127.0.0.1:7000` under the status table; `azemu status` prints it too. It shows only this project's containers, named by their app key (`func-writer`, `web-backend`, `proxy`, `azurite`, `logs`), with live tail, search, level highlighting and JSON pretty-printing. Select all containers (or open the stack group) for one merged, colour-coded stream; the proxy logs one `METHOD host path status ms` line per request, handy for tracing calls between services. Stopped containers stay listed, so a crashed app's last logs remain readable.

The port is 7000 when free (else a free one is picked), kept in `.azemu/ports.json` under `_logs`. If the dashboard fails to start, `up` only warns.

## Pull real data into the emulator

With the stack running (`azemu up`), copy named containers and tables from the real storage account into the emulated one of the same name:

```
azemu storage pull stmyapp --containers prompts,docs --prefix 2024/ --tables Settings
azemu storage pull stmyapp --all          # every container and table
```

It reads the account key with your `az login` (`az storage account keys list`), the same path as the portal's "Access key" mode, so no data-plane RBAC is needed. The key is only held in memory. Blobs are streamed, overwritten, and skipped when size and Content-MD5 already match; table entities keep their types. Flags: `--subscription S`, `--env E`, `-f dir`, `--concurrency 8`.

- Needs a role that can list keys (Contributor or Storage Account Key Operator): activate your PIM role first.
- Real data lands on your laptop (the Azurite volume); `azemu down --volumes` wipes it.
- Only what you name (or `--all`) is copied. Queues are not (ephemeral). Azure is only read, never written.

## How it works

```
host                                   docker network "azemu-<project>"
azemu CLI (Go)                         azemu-proxy  (aliases: every emulated hostname)
 ├ terraform plan -> cached model        ├ :443   TLS by SNI, leaf certs from a local CA
 ├ settings resolver (+ .env)            │        <app>.azurewebsites.net -> app container
 ├ generates .azemu/compose.yaml         │        <acct>.{blob,queue,table}.core.windows.net -> azurite
 ├ docker compose build/up (parallel)    ├ :8081  fake managed identity (IDENTITY_ENDPOINT)
 └ watch loop -> batched rebuilds        └ :7001… ingress per app on localhost, injects Easy Auth headers
                                       azurite  (HTTPS, --oauth basic, one account per storage account)
                                       app containers: resolved settings + CA bundle vars
```

Settings are not rewritten: real Azure hostnames resolve to the proxy inside the network and the local CA is trusted. Key Vault references are replaced from `.env`, storage keys by the Azurite key, App Insights is blanked.
`up` runs plan, image builds and platform startup concurrently; a failed build of one app does not stop the others, and in watch mode a failed rebuild leaves the old container serving. Warm `up` (cached images and plan) takes seconds.

## Microsoft Graph with your own login

If an app calls Graph (SharePoint, OneDrive...) with an app registration (`ClientSecretCredential`, client id + secret), add `graph: {}` to `azemu.yaml` and run `azemu graph login` once (device code). The app is unchanged: azemu answers its client-credentials request for `https://graph.microsoft.com/.default` with a fake token, then swaps in **your** delegated token on `graph.microsoft.com`. Token requests for any other resource go to the real Entra untouched, and the client secret is never used.

```yaml
graph:
  scopes: [Sites.Read.All, Files.Read.All]   # default; delegated scopes asked at login
  writable: false                            # default: POST/PATCH/PUT/DELETE are rejected (reads, search/query and GET-only $batch pass)
  # tenant: organizations   client_id: <public client; default: Microsoft Graph Command Line Tools>
```

`azemu graph status` shows the signed-in account. The refresh token lives in `.azemu/graph/` (mode 0600). A tenant may block the default client id for consent; then register your own public client with delegated permissions and set `client_id`. `azemu logs azemu-proxy` shows every Graph call (`grep graph.microsoft.com`).

Delegated is not app-only: you get your own access, so missing app role grants, admin consent and `Sites.Selected` grants are not caught, and `/me` works here but not in Azure.

## What it can't catch

- RBAC: the fake identity can do anything on emulated storage, so missing role assignments will not show up.
- VNet, private endpoints, firewalls, DNS.
- Easy Auth token validation: the proxy injects the headers for `--as`; it does not run the Entra flow.
- Cold start, scaling, quotas, Windows plans, Flex Consumption specifics.
- External services (OpenAI, data warehouses...) are real and called as-is; Graph is too, with your login (see above).

## Current limits

- Zip apps: python and node only. Docker apps use your Dockerfile as-is.
- The plan needs access to the Terraform state, or set `infra.plan_file` to a `terraform show -json` file (or a saved plan) to skip running terraform.
- Values only known after apply (e.g. a function URL) must be set under `apps.<name>.env` in `azemu.yaml`; `azemu env <app>` warns about them.
- Azure Functions images are amd64-only, so zip apps run under emulation on Apple Silicon.
