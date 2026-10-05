# Run a private network on your own computer

This guide sets up AyeusANN for you and people you know: the platform runs on
one computer of yours, friends' machines join it with their GPUs, and everyone
calls the models through one address.

It is a real installation, not `make dev`. It has its own generated secrets, its
own database, and backups, and it comes back by itself after a reboot. `make
dev` keeps working beside it, on its own ports and its own data.

| | `make dev` | Private network (`make private-up`) |
|---|---|---|
| Secrets | Published in this repository | Generated on your computer, kept outside the repository |
| Database | Development data, reachable on this computer | Its own, reachable only from inside the stack |
| Address | Follows each request | Fixed, chosen by you |
| Ports open | 8080 and the services' own | One: 8090 |
| Backups | None | Daily, checked, 14 daily and 8 weekly kept |
| Simulated GPUs | Allowed | Refused |
| After a reboot | Start it again | Comes back with Docker |

## What you need

- Docker Desktop (macOS, Windows) or Docker Engine with Compose (Linux).
- In Docker Desktop: Settings > General > **Start Docker Desktop when you sign
  in**. Without it the platform stays down after a reboot until you open Docker.
- About 2 GB of disk for the images and under 1 GB of memory.

## 1. Create the installation

```bash
make private-init
```

This writes `~/.ayeusann-platform/private.env` (readable only by you) with
freshly generated secrets, and picks this computer's address on your network:

```
  Address    http://192.168.1.4:8090
  Backups    /Users/you/.ayeusann-platform/backups
```

**Keep `private.env`.** It holds the keys of this installation: the one that
signs sign-in sessions, and the one that signs every job sent to a host, which
hosts pin when they enrol. If the file is lost, every account has to sign in
again and every machine has to enrol again. A copy is saved beside every
database backup. The command never overwrites an existing file.

To use a different address or port from the start:

```bash
scripts/private.sh init --address http://100.101.102.103:8090    # for example a VPN address
```

See [Choosing the address](#choosing-the-address) before you invite anyone.

## 2. Start it

```bash
make dist-agent            # the agent for this computer's OS, for the installer to download
make private-up
```

The first start builds the images, which takes a few minutes. It ends with:

```
  Running    http://192.168.1.4:8090   (on this computer: http://localhost:8090)
  Backups    last at 2026-10-05T15:36:47Z, in /Users/you/.ayeusann-platform/backups
```

Open that address, sign up, and you are in. `make private-status` prints the
same summary at any time.

Publish the agent for each kind of machine that will join, as in
[connect-a-gpu.md](connect-a-gpu.md#2-publish-the-agent-for-the-hosts-operating-system):
`make dist-agent-linux`, `make dist-agent-windows`.

On macOS the firewall may ask whether Docker may accept incoming connections.
Allow it, or other machines cannot reach the platform.

> Until invitations are added (the next milestone), anyone who can reach the
> address can create an account. Keep it on your own network or a private VPN,
> and do not forward the port from your router or expose it through a public
> tunnel yet.

## 3. Connect machines

Exactly as in [connect-a-gpu.md](connect-a-gpu.md#3-connect-the-host), using
this installation's address: open the console, go to Machines > Add a machine,
and run the command it gives on the machine with the GPU. That includes this
computer, if it has a GPU of its own.

Two things differ from development:

- **Machines join as Tier 3 (personal).** Only the operator can enrol a machine
  at a higher tier.
- **A machine that was enrolled with `make dev` must enrol again**, once. Its
  agent pinned the development signing key, and this installation has its own
  key and its own database. Run this installation's install command on it.

## Choosing the address

Every machine and every person uses one address, and it is written into the
install commands and the endpoints the console hands out. It has to keep
working, so pick one that does not change.

| Choice | Good for | Catch |
|---|---|---|
| **A private VPN** such as [Tailscale](https://tailscale.com) on every machine | Friends in different places. Private: nobody outside the VPN can reach the sign-in page. The address never changes. | Everyone installs the VPN. |
| **Your computer's address on the local network** (the default) | Machines in the same home or lab. | The address can change when the router restarts. Reserve it for this computer in the router's settings. |
| **A name you own**, pointing at a public address or tunnel | People who cannot install anything. | The sign-in page is on the internet. Wait for invitations before doing this. |

### Moving to a new address

```bash
scripts/private.sh address http://100.101.102.103:8090
```

This changes the address and restarts the services. Machines that are connected
are told the new address and use it by themselves once the old one stops
answering, so nobody has to reinstall anything. Keep the old address working
until `ayeusann-agent service status` on each machine lists the new one. A
machine that was switched off through the whole change needs the address set
once by hand:

```bash
ayeusann-agent service install --coordinator http://100.101.102.103:8090
```

Endpoints already handed to people still name the old address; new ones use the
new address.

## Backups

A dump of the database is taken a few minutes after the first start and then
whenever the newest one is more than 24 hours old. It is not tied to a time of
day, because a laptop is usually asleep at 3 a.m.; a computer that was off for
a week takes one a few minutes after it is back. Every dump is read back before
it counts.

```
~/.ayeusann-platform/backups/
  daily/            the last 14
  weekly/           the last 8 (the first dump of each week)
  before-upgrade/   taken before an update changes the database's structure
  before-restore/   taken before a restore replaces the database
  private.env       a copy of the secrets file
  last-backup       when the last dump succeeded
```

`make private-status` shows when the last dump was taken, and says so if dumps
are failing.

**These files are on the same disk as the database.** A dead disk takes both.
Point `BACKUP_DIR` in `private.env` at a folder that is copied elsewhere (a
cloud-synced folder, an external disk, Time Machine), then `make private-up`.
The dumps contain password hashes and the `private.env` copy contains the
installation's keys, so choose somewhere only you can read.

| Task | Command |
|---|---|
| Take a dump now | `make private-backup` |
| Check that a dump restores (changes nothing) | `make private-restore FILE=~/.ayeusann-platform/backups/daily/<file>.dump` |
| Replace the database with a dump | `scripts/private.sh restore <file>.dump --replace` |

The check restores the dump into a scratch database and prints what it holds:
schema version, number of accounts, machines and deployments, and the newest
account. Do this once in a while; a backup you have never restored is a hope,
not a backup.

Replacing asks you to type `REPLACE`, saves the current database to
`before-restore/` first, proves the dump restores before dropping anything, and
brings the services back whether or not it worked.

### Moving to another computer

1. If the new computer will have a different address, announce it from the old
   one first, while it is still running, so connected machines learn it:
   `scripts/private.sh address http://new-address:8090`. Then take a dump:
   `make private-backup`.
2. On the new computer: clone this repository and copy
   `~/.ayeusann-platform/private.env` into place (the copy in the backups
   folder is the same file).
3. `make dist-agent` (and the other agent builds), then `make private-up`.
4. `scripts/private.sh restore <newest dump> --replace`.
5. Stop the old one: `make private-down`. Machines move over by themselves.

## Updating

```bash
git pull
make dist-agent            # and the other agent builds you publish
make private-up
```

`private-up` rebuilds the images and restarts. If the update changes the
database's structure, a dump is taken to `before-upgrade/` before anything is
applied.

## Operating

| Task | Command |
|---|---|
| Health, address, last backup | `make private-status` |
| Logs | `make private-logs`, or `scripts/private.sh logs coordinator` |
| Stop (data is kept) | `make private-down` |
| Start | `make private-up` |
| Any Compose command | `scripts/private.sh compose ps` |
| A database shell | `scripts/private.sh compose exec postgres psql -U ayeusann ayeusann` |

Settings live in `~/.ayeusann-platform/private.env`; `make private-up` applies
a change. `PLATFORM_ADMIN_EMAILS` lists the accounts that see the operations
console.

To remove the installation entirely, including its database:
`scripts/private.sh compose down -v`, then delete `~/.ayeusann-platform`.

## What is and is not protected

- The database, Redis and the internal services are not reachable from your
  network, only from inside the stack. One port is open: the gateway's.
- Traffic is **not encrypted** between machines and the platform. On a home
  network or a VPN (which encrypts by itself) that is acceptable; across the
  open internet it is not. A public installation uses the production stack with
  TLS: [deployment.md](deployment.md).
- The owner of a machine that serves a model can, with enough effort, see the
  requests it serves. Share a network with people you would share that with.
- Sleeping this computer takes the platform offline. Machines reconnect by
  themselves when it is back.

## Checking the whole thing

```bash
make private-check
```

creates a throwaway installation next to yours (its own secrets, database,
project name and port 8091), starts it, runs both end-to-end tests with no GPU,
takes a backup, restores it, replaces the database from it, feeds it a damaged
dump, checks that only one port is published and that a simulated GPU is
refused, and removes everything again. CI runs it on every push.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| "No installation found" | Run `make private-init` first. |
| "Docker is not running" | Start Docker Desktop. |
| Other machines cannot open the address | A firewall on this computer is blocking port 8090, the machines are on different networks, or the address changed. `make private-status` shows the address in use. |
| "this agent is reporting a simulated GPU" | The agent was started with `--fake-gpu` or `SN_FAKE_GPU`. That is for development only. |
| A machine enrolled under `make dev` is refused or ignores jobs | It pinned the development keys. Run a new install command from this installation's console on it. |
| `make private-status` says backups are failing | `scripts/private.sh logs backup` shows why; a full disk is the usual cause. |
| Port 8090 is taken | Set `PRIVATE_PORT` in `private.env`, then `scripts/private.sh address http://<address>:<new port>`. |
