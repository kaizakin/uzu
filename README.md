# Unikernel SQLite API experiment

This repo is a small test workload for running a plain Go service inside a Unikraft unikernel through `urunc`.

The app itself is intentionally simple: one static binary, one HTTP API, one local SQLite database. The point of the project is everything around the app: static linking, rootfs layout, `elfloader`, `bunny`, `urunc`, and the networking path into the guest.

## What it does

The build produces a single binary:

```text
build/sqlite-api
```

That binary starts an HTTP server on port `8080` and stores data in:

```text
/data/app.db
```

In this repo, `/data` comes from:

```text
rootfs/data
```

The API is just a key/value store. That is enough to verify a few important things:

- the unikernel boots the Go binary,
- the binary accepts HTTP traffic,
- SQLite can create and update a local database file,
- the filesystem layout inside the guest is what I expect,
- state survives for as long as the chosen rootfs or storage setup allows.

## API

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/healthz` | Confirms the process is up and SQLite responds |
| `GET` | `/items` | Lists all items |
| `POST` | `/items` | Creates or replaces an item using `key` and `value` |
| `GET` | `/items/{key}` | Returns one item |
| `PUT` | `/items/{key}` | Creates or replaces one item by key |
| `DELETE` | `/items/{key}` | Deletes one item |

Example:

```bash
curl -sS -X POST http://127.0.0.1:8080/items \
  -H 'content-type: application/json' \
  -d '{"key":"hello","value":"unikraft"}'

curl -sS http://127.0.0.1:8080/items/hello
```

## How the image is put together

The flow looks like this:

```text
Go API
  -> static linux/amd64 binary
  -> minimal rootfs with /sqlite-api and /data
  -> Unikraft elfloader
  -> bunny image
  -> urunc
```

The project uses `modernc.org/sqlite` instead of a CGO-based SQLite driver so the binary can be built with:

```bash
CGO_ENABLED=0
```

That keeps the output fully static and avoids a dynamic linker. For this setup, a binary with a `PT_INTERP` segment is a bad build.

You can check that with:

```bash
make verify-static
```

Expected output should include something like:

```text
ELF 64-bit LSB executable, x86-64, statically linked, stripped
```

## Root filesystem layout

The rootfs is intentionally small:

```text
rootfs/
  data/
  etc/
```

Once packaged, the important paths inside the guest are:

```text
/sqlite-api
/data/app.db
/etc
```

`/sqlite-api` is copied from:

```text
build/sqlite-api
```

The database path defaults to `/data/app.db`. You can override it either with:

```bash
DB_PATH=/somewhere/app.db
```

or:

```bash
/sqlite-api -db=/somewhere/app.db
```

The listen address defaults to `:8080`. If `PORT` is set and the default listen address is still in use, the service will bind to `:$PORT`.

## Local development

For normal local development:

```bash
make run
```

That runs the API directly on the host and uses `./app.db`, which is faster for basic API testing than booting a unikernel every time.

For the static build:

```bash
make static
make verify-static
```

The output binary is:

```text
build/sqlite-api
```

## Packaging for Unikraft and `urunc`

The packaging entry point is `bunnyfile`.

It currently includes:

```text
build/sqlite-api -> /sqlite-api
rootfs/data      -> /data
rootfs/etc       -> /etc
```

The kernel section expects an initrd-capable Unikraft elfloader at:

```text
build/elfloader_qemu-x86_64
```

That artifact does not come from the Go build. It has to be built or supplied separately from the Unikraft side.

Once the elfloader is available:

```bash
make static
bunny build -f bunnyfile -t sqlite-api-unikernel
```

The image starts the service with:

```text
/sqlite-api -listen=:8080 -db=/data/app.db
```

## Networking notes

This is still the part under active investigation.

The service itself is straightforward: it listens on `:8080`. The harder question is how traffic reaches that listener once the process is no longer a regular Linux process and is instead running as a unikernel behind `urunc`.

The current `bunnyfile` uses this network fragment:

```text
netdev.ip=172.44.0.2/24:172.44.0.1:172.44.0.1::sqlite-api:local
```

That is runtime plumbing, not application logic. The exact values will likely change depending on the environment, CNI setup, runtime class, and whether the workload is being tested directly with `urunc`, through containerd, or in a Kubernetes or Knative path.

What I care about here is:

- whether host-to-guest traffic arrives at all,
- what source address the service sees,
- whether normal loopback assumptions stop applying,
- whether a sidecar or proxy can reach the service,
- whether `/data/app.db` is writable once the guest is running.

This repo used to expose a metrics endpoint for tracing request paths and source IPs. I switched to SQLite because it gives a more useful end-to-end signal: networking plus local writes.

## Current status

Working now:

- the Go API is implemented,
- SQLite works locally,
- the static build works with `CGO_ENABLED=0`,
- `make verify-static` confirms there is no dynamic interpreter,
- `bunnyfile` points at the static binary and the minimal rootfs layout.

Still in progress:

- restoring or rebuilding `build/elfloader_qemu-x86_64`,
- validating the full `bunny build` path once the elfloader is present,
- running the image through `urunc`,
- testing the host or sidecar networking path into the guest,
- deciding what persistence setup makes sense for `/data/app.db` in the real runtime.

## Useful commands

```bash
make run
make static
make verify-static
make rootfs
make image
```
