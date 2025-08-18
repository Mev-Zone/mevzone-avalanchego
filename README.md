<div align="center">
  <img src="resources/AvalancheLogoRed.png?raw=true">
</div>

---

Node implementation for the [Avalanche](https://avax.network) network -
a blockchains platform with high throughput, and blazing fast transactions.

## Installation

Avalanche is an incredibly lightweight protocol, so the minimum computer requirements are quite modest.
Note that as network usage increases, hardware requirements may change.

The minimum recommended hardware specification for nodes connected to Mainnet is:

- CPU: Equivalent of 8 AWS vCPU
- RAM: 16 GiB
- Storage: 1 TiB
  - Nodes running for very long periods of time or nodes with custom configurations may observe higher storage requirements.
- OS: Ubuntu 22.04/24.04 or macOS >= 12
- Network: Reliable IPv4 or IPv6 network connection, with an open public port.

If you plan to build AvalancheGo from source, you will also need the following software:

- [Go](https://golang.org/doc/install) version >= 1.23.9
- [gcc](https://gcc.gnu.org/)
- g++

### Building From Source

#### Clone The Repository

Clone the AvalancheGo repository:

```sh
git clone git@github.com:Mev-Zone/mevzone-avalanchego.git
cd mevzone-avalanchego
```

This will clone and checkout the `master` branch.

#### Building AvalancheGo

Build AvalancheGo by running the build task:

```sh
./scripts/run_task.sh build
```

The `avalanchego` binary is now in the `build` directory. To run:

```sh
./build/avalanchego
```

### Activating MEV client

The MEV auction logic is enabled per chain.
AvalancheGo reads a configuration file stored at

```sh
~/.avalanchego/configs/chains/<blockchainID>/config.json
```

Replace <blockchainID> with the ID of the chain you want to serve.
For Mainnet C-Chain the ID is:

```sh
2q9e4r6Mu3U68nU1fYjgbR6JvwrRx36CohpAX5UQxse55x1Q5
```

#### 1. Create the directory
```sh
export CHAIN_ID=2q9e4r6Mu3U68nU1fYjgbR6JvwrRx36CohpAX5UQxse55x1Q5
mkdir -p "$HOME/.avalanchego/configs/chains/$CHAIN_ID"
```

#### 2. Save the file exactly as
```sh
$HOME/.avalanchego/configs/chains/$CHAIN_ID/config.json
```
```json
{
  // Turns the MEV auction logic on for this validator.
  "mev-api-enabled": true,
  // MEV parameters
  "mev": {
    // Whitelisted builder relay(s) – address must match the signer of the bundles it relays.
    "builders": [
      {
        "address": "0x00B22a6A183DdBefe7B515A73eC2Dc7C39bf82cE",
        "url": "https://builder.mev.zone/ext/bc/C/rpc"
      }
    ],
    // Commission the validator keeps from every winning bundle (basis-points).
    "validatorCommission": 500,
    // Address that actually receives the commission.
    "validatorWallet": "0x00..."
  }
}
```

#### 3. Start AvalancheGo
```sh
./build/avalanchego            # the daemon automatically picks up ~/.avalanchego/configs/...
```
