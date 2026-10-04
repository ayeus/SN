# Use a GPU on another machine

This guide connects a GPU in one computer to the platform running on another,
and calls the model from a third (or from either of the first two).

Three roles are involved. One computer can play more than one.

| Role | What it runs | Example |
|---|---|---|
| **Platform** | `make dev`: the console, the API and the coordinator | Your laptop |
| **Host** | The agent and Ollama, next to the GPU | A friend's gaming laptop |
| **Client** | Your code, with the OpenAI SDK | Any laptop |

The host only makes outbound connections to the platform, so nothing has to be
opened or forwarded on the host's network. The platform is the machine that
has to be reachable.

## 1. Start the platform

On the platform machine:

```bash
make dev
```

It ends by printing the address other machines use:

```
  From other machines on this network: http://192.168.1.4:8080
  (agents connect to 192.168.1.4:50051; allow incoming connections if your firewall asks)
```

`make status` prints it again. If the line is missing, the machine is not on a
network.

On macOS, the firewall may ask whether `gateway` and `coordinator` may accept
incoming connections. Allow both. If you dismissed the prompt, allow them under
System Settings > Network > Firewall > Options.

## 2. Publish the agent for the host's operating system

The install command downloads a prebuilt agent from the platform. Build the one
the host needs, once, on the platform machine:

| Host | Command | Needs |
|---|---|---|
| Windows 10 or 11 (64-bit) | `make dist-agent-windows` | Docker |
| Linux x86-64 | `make dist-agent-linux` | Docker |
| Linux ARM64 | `make dist-agent-linux ARCH=arm64` | Docker |
| The same kind of machine as the platform | `make dist-agent` | Rust |

The binaries land in `dist/agent/` and are served at `/downloads/`.

## 3. Connect the host

On the host machine:

1. Install [Ollama](https://ollama.com/download) and leave it running.
2. NVIDIA GPUs need driver 535 or newer. Apple Silicon needs nothing extra.

Then, in the console (on any machine; open the address from step 1 and sign
in):

1. Go to **Host > Add a machine**.
2. Choose the kind of machine and click **Create install command**.
3. Pick the tab for the host's operating system, copy the command, and run it
   on the host: in a terminal on macOS or Linux, in PowerShell on Windows.

The command installs the agent, enrols the machine with a single-use token and
starts it. The console shows the machine within a minute. Keep the agent
running; to start it again later, run the agent with no arguments (the installer
prints the path).

## 4. Deploy and call it

1. In the console, go to **Deploy > Models** and deploy a model on T3.
2. Watch it reach **Serving**. The first deployment of a model downloads its
   weights on the host, which can take a few minutes.
3. Copy the base URL and API key from the deployment page and call it from any
   machine that can reach the platform:

```python
from openai import OpenAI

client = OpenAI(base_url="http://192.168.1.4:8080/v1", api_key="sk_live_...")
client.chat.completions.create(model="<deployment-name>", messages=[{"role": "user", "content": "Hi"}])
```

The request goes to the platform, which forwards it to the host over the
agent's own connection. The client never talks to the host directly.

### Choosing which GPU serves

The scheduler places a deployment on the best eligible GPU that is free. To
make sure a particular machine serves it, pause the others: open the machine
under **Host > Machines** and click **Pause**.

## Machines on different networks

Step 1 assumes all machines share a Wi-Fi or LAN. When they do not, the host
and the client need some other route to the platform.

**A private VPN (simplest).** Install [Tailscale](https://tailscale.com) or a
similar mesh VPN on every machine and join them to one network. Then open the
console using the platform's VPN address, for example
`http://100.101.102.103:8080`, and create the install command from there. The
command carries whichever address the console was opened with.

**A fixed address.** Put the addresses in `.env` on the platform machine and
restart it:

```bash
PUBLIC_URL=http://203.0.113.10:8080
COORDINATOR_PUBLIC_URL=http://203.0.113.10:50051
```

Ports 8080 and 50051 must then be reachable at that address. Traffic is not
encrypted in development mode, so use this only on networks you trust.

**A public server.** For hosts and customers on the open internet, run the
production stack, which adds TLS: see [deployment.md](deployment.md).

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| The console warns the command "only works on this computer" | The platform machine has no network address. Connect it to a network, or open the console by an address the host can reach, then create a new command. |
| The address printed is a VPN's, or missing while on Wi-Fi | Set `PUBLIC_URL` and `COORDINATOR_PUBLIC_URL` in `.env` to the right address. |
| The installer cannot download the agent | Step 2 was skipped for this operating system. On macOS and Linux the installer falls back to building from source if it is run inside a checkout of this repository with Rust installed. |
| The host cannot connect at all | A firewall on the platform machine is blocking ports 8080 and 50051, or the machines are on different networks (guest Wi-Fi often isolates devices from each other). |
| The machine is online but shows "Runtime not reachable" | Ollama is not running on the host. Start it. |
| "no supported GPU found" | The NVIDIA driver is missing or `nvidia-smi` is not on the PATH. |
| "registration rejected" | The token was already used or is older than 24 hours. Create a new command. |
| The deployment stays in "Waiting for capacity" | No free GPU qualifies. The deployment page says why: usually the model needs more GPU memory than the host has, or the host is paused. |

## What has been tested

`scripts/smoke-remote.sh` runs this whole flow automatically. A clean Linux
container plays the host: it enrols with the console's one-line command, serves
a deployment, and is called through the platform's network address.

The Windows agent is cross-compiled and has been run under Wine far enough to
start, read its arguments and report a missing GPU. It has not yet been run on
a real Windows machine with an NVIDIA GPU; treat the first such run as a test.
