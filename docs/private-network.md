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
  Owner      Not set up yet. Create your account, the first one, here:
             http://localhost:8090/signup?owner=3f9c1e0a77b2-5d1048c2e6aa
  Backups    last at 2026-10-05T15:36:47Z, in /Users/you/.ayeusann-platform/backups
```

Open the owner link and create your account. The code in it works once: the
account it creates is the **operator**, the person who invites everyone else
and sees every machine. After that the line disappears.

`make private-status` prints the same summary at any time.

Publish the agent for each kind of machine that will join, as in
[connect-a-gpu.md](connect-a-gpu.md#2-publish-the-agent-for-the-hosts-operating-system):
`make dist-agent-linux`, `make dist-agent-windows`.

On macOS the firewall may ask whether Docker may accept incoming connections.
Allow it, or other machines cannot reach the platform.

## 3. Invite people

Nobody can create an account by finding the address. Accounts come from
invitation links, and only you make those: in the console, open **Operations**
and use **Invitations**.

- Each link works once and lasts three days. Send it yourself, in whatever way
  you already talk to the person.
- **A workspace of their own** is the usual choice: their deployments, keys and
  usage are theirs. **A seat in your workspace** shares yours.
- Naming an email address means only that address can use the link.
- A link you regret can be withdrawn until it is used.

The same page lists everyone with an account. For each person you can:

- **Create a reset link.** A computer at home has no mail server, so "Forgot
  your password?" cannot email anyone. You make the link and pass it on; it
  works once, for 24 hours.
- **Disable the account.** They are signed out at once and cannot sign in until
  you enable it again. If nobody else can sign in to their workspace, its API
  keys stop working for good. Their machines are not touched; drain or ban those
  in the fleet list on the same page.

If it is you who forgot the password, this computer is the way back in:

```bash
scripts/private.sh reset-link you@example.com
```

Sign-in is limited to ten wrong attempts in ten minutes per address and email.
Once reached, even the right password waits out the ten minutes.

## 4. Connect machines

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
| **A name you own**, pointing at a public address or tunnel | People who cannot install anything. | The sign-in page is on the internet and traffic to it is not encrypted unless the tunnel does it. |

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

## Updating the agents on other people's machines

Once a machine is enrolled you cannot reach into it, so the agent updates
itself. You publish a release; each machine downloads it, checks that it is
yours, and switches over.

**Once, before you invite anyone:**

```bash
scripts/private.sh release-key
make private-up
```

This creates `~/.ayeusann-platform/release.key` and tells the platform its
public half. **Copy `release.key` somewhere off this computer** (a password
manager, a USB stick). Machines pin its public half when they enrol and accept
updates signed by that key and no other. If the key is lost, no update can
reach them; each would have to be enrolled again. The platform itself never
holds the key, so someone who broke into the platform could stop updates but
could not push software to your friends' machines.

**Each time you want to roll out a new version:**

```bash
git pull
make dist-agent dist-agent-linux dist-agent-windows    # the builds you publish
scripts/private.sh release
```

Connected machines update within a minute or so; the rest when they next
connect. Operations shows each machine's agent version.

What a machine does with a release:

1. Checks the release's signature against the key it pinned. Anything else is
   refused.
2. Downloads the build for its platform and checks its size and checksum.
3. Runs the new build once to see that it starts and is the version it claims.
4. Waits for answers in progress to finish, swaps the files (the old build is
   kept beside the new one) and starts the new one. Models stay loaded.
5. If the new build keeps failing to connect for ten minutes, or keeps
   crashing at start, the old one is put back. That version is left alone for
   a day and then tried again, in case the platform was the problem. Nothing
   older than a version the machine has already installed is ever accepted.

To stop giving work to machines on old versions, name the oldest you accept:

```bash
scripts/private.sh release 0.4.0
```

Older agents are not turned away, because then they could never update. They
stay connected, show as "out of date", and are given no new work until they
have updated. What they are already serving carries on, so a machine in the
middle of updating does not drop its models.

A machine pins the release key when it enrols, and never takes one from the
platform afterwards. A machine that enrolled before you ran `release-key` will
say so in its log and ignore updates; run its install command again, or start
its agent once with `--release-key <the public key>`.

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
a change. `SIGNUP_MODE` is `invite` by default; `closed` stops new accounts
altogether, and `open` lets anyone who can reach the address sign up.

To remove the installation entirely, including its database:
`scripts/private.sh compose down -v`, then delete `~/.ayeusann-platform`.

## What is and is not protected

- Only people you invited have accounts, and only your account can invite,
  disable accounts or see everyone's machines.
- The database, Redis and the internal services are not reachable from your
  network, only from inside the stack. One port is open: the gateway's.
- Traffic is **not encrypted** between machines and the platform. On a home
  network or a VPN (which encrypts by itself) that is acceptable; across the
  open internet it is not. A public installation uses the production stack with
  TLS: [deployment.md](deployment.md).
- The owner of a machine that serves a model can, with enough effort, see the
  requests it serves. Share a network with people you would share that with.
- Sleeping this computer takes the platform offline. Machines keep their
  models loaded and reconnect by themselves when it is back, so nothing is
  reloaded after a nap. A machine that cannot reach the platform for ten
  minutes unloads its models and gives the GPU's memory back to its owner;
  they are loaded again when the platform returns.

## Checking the whole thing

```bash
make private-check
```

creates a throwaway installation next to yours (its own secrets, database,
project name and port 8091), starts it, runs both end-to-end tests with no GPU,
checks that nobody can sign up uninvited and that the owner code makes exactly
one operator, takes a backup, restores it, replaces the database from it, feeds
it a damaged dump, checks that only one port is published and that a simulated
GPU is refused, and removes everything again. CI runs it on every push.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| "No installation found" | Run `make private-init` first. |
| A machine shows "Agent out of date" and stays that way | It could not update itself (see `ayeusann-agent service status` and its log on that machine). Running the install command on it again installs the current build. |
| "Accounts on this network are by invitation" | Sign-up needs an invitation link from the operator. The very first account uses the owner link that `make private-status` prints. |
| "This installation already has its owner" | The owner code was already used. Ask that person for an invitation, or see `scripts/private.sh reset-link` if it was you. |
| "Docker is not running" | Start Docker Desktop. |
| Other machines cannot open the address | A firewall on this computer is blocking port 8090, the machines are on different networks, or the address changed. `make private-status` shows the address in use. |
| "this agent is reporting a simulated GPU" | The agent was started with `--fake-gpu` or `SN_FAKE_GPU`. That is for development only. |
| A machine enrolled under `make dev` is refused or ignores jobs | It pinned the development keys. Run a new install command from this installation's console on it. |
| `make private-status` says backups are failing | `scripts/private.sh logs backup` shows why; a full disk is the usual cause. |
| Port 8090 is taken | Set `PRIVATE_PORT` in `private.env`, then `scripts/private.sh address http://<address>:<new port>`. |
