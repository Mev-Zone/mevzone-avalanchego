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

- [Go](https://golang.org/doc/install) version >= 1.25.10
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

## Running Avalanche

### Connecting to Mainnet

To connect to the Avalanche Mainnet, run:

```sh
./build/avalanchego
```

You should see some pretty ASCII art and log messages.

You can use `Ctrl+C` to kill the node.

### Connecting to Fuji

To connect to the Fuji Testnet, run:

```sh
./build/avalanchego --network-id=fuji
```

### Creating a Local Testnet

The [avalanche-cli](https://github.com/ava-labs/avalanche-cli) is the easiest way to start a local network.

```sh
avalanche network start
avalanche network status
```

## Bootstrapping

A node needs to catch up to the latest network state before it can participate in consensus and serve API calls. This process (called bootstrapping) currently takes several days for a new node connected to Mainnet.

A node will not [report healthy](https://build.avax.network/docs/api-reference/health-api) until it is done bootstrapping.

Improvements that reduce the amount of time it takes to bootstrap are under development.

The bottleneck during bootstrapping is typically database IO. Using a more powerful CPU or increasing the database IOPS on the computer running a node will decrease the amount of time bootstrapping takes.

## Generating Code

AvalancheGo uses multiple tools to generate efficient and boilerplate code.

### Running protobuf codegen

To regenerate the protobuf go code, run `scripts/run-task.sh generate-protobuf` from the root of the repo.

This should only be necessary when upgrading protobuf versions or modifying .proto definition files.

To use this script, you must have [buf](https://docs.buf.build/installation) (v1.31.0), protoc-gen-go (v1.33.0) and protoc-gen-go-grpc (v1.3.0) installed.

To install the buf dependencies:

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.33.0
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.3.0
```

If you have not already, you may need to add `$GOPATH/bin` to your `$PATH`:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

If you extract buf to ~/software/buf/bin, the following should work:

```sh
export PATH=$PATH:~/software/buf/bin/:~/go/bin
go get google.golang.org/protobuf/cmd/protoc-gen-go
go get google.golang.org/protobuf/cmd/protoc-gen-go-grpc
scripts/run_task.sh generate-protobuf
```

For more information, refer to the [GRPC Golang Quick Start Guide](https://grpc.io/docs/languages/go/quickstart/).

### Running mock codegen

See [the Contributing document autogenerated mocks section](CONTRIBUTING.md####Autogenerated-mocks).

## Documentation

In-repo documentation conventions are described in
[`docs/documentation-guidelines.md`](docs/documentation-guidelines.md). New
feature documentation, package-level docs, and maintainer-oriented context
should follow that guide.

## Versioning

### Version Semantics

AvalancheGo is first and foremost a client for the Avalanche network. The versioning of AvalancheGo follows that of the Avalanche network.

- `v0.x.x` indicates a development network version.
- `v1.x.x` indicates a production network version.
- `vx.[Upgrade].x` indicates the number of network upgrades that have occurred.
- `vx.x.[Patch]` indicates the number of client upgrades that have occurred since the last network upgrade.

### Library Compatibility Guarantees

Because AvalancheGo's version denotes the network version, it is expected that interfaces exported by AvalancheGo's packages may change in `Patch` version updates.

AvalancheGo is organized as multiple Go modules that must be consumed together at matching versions. For guidance on depending on these modules in your Go projects, see [Depending on AvalancheGo Modules](docs/external_consumption.md).

### API Compatibility Guarantees

APIs exposed when running AvalancheGo will maintain backwards compatibility, unless the functionality is explicitly deprecated and announced when removed.

## Supported Platforms

AvalancheGo can run on different platforms, with different support tiers:

- **Tier 1**: Fully supported by the maintainers, guaranteed to pass all tests including e2e and stress tests.
- **Tier 2**: Passes all unit and integration tests but not necessarily e2e tests.
- **Tier 3**: Builds but lightly tested (or not), considered _experimental_.
- **Not supported**: May not build and not tested, considered _unsafe_. To be supported in the future.

The following table lists currently supported platforms and their corresponding
AvalancheGo support tiers:

| Architecture | Operating system | Support tier  |
| :----------: | :--------------: | :-----------: |
|    amd64     |      Linux       |       1       |
|    arm64     |      Linux       |       1       |
|    arm64     |      Darwin      |       2       |
|    amd64     |      Darwin      | Not supported |
|    amd64     |     Windows      | Not supported |
|     arm      |      Linux       | Not supported |
|     i386     |      Linux       | Not supported |

To officially support a new platform, one must satisfy the following requirements:

| AvalancheGo continuous integration | Tier 1  | Tier 2  | Tier 3  |
| ---------------------------------- | :-----: | :-----: | :-----: |
| Build passes                       | &check; | &check; | &check; |
| Unit and integration tests pass    | &check; | &check; |         |
| End-to-end and stress tests pass   | &check; |         |         |

## Security Bugs

**We and our community welcome responsible disclosures.**

Please refer to our [Security Policy](SECURITY.md) and [Security Advisories](https://github.com/ava-labs/avalanchego/security/advisories).

## Licenses

Unless otherwise stated, all code in this repository is licensed under BSD-3. See our [licensing](LICENSE) for more details. Specifically, [grafted repositories](graft/) may use
a different license, and one must refer to that project's README and LICENSE for more details
