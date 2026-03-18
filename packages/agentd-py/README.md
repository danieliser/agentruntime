# agentruntime-agentd

Pre-built `agentd` binary for running coding agents behind one API. No Go toolchain required.

## Installation

```bash
pip install agentruntime-agentd
```

## Usage

### Command line

After installation, `agentd` is available directly:

```bash
agentd --port 8090 --runtime local
```

### Programmatic

```python
from agentruntime_agentd import get_binary_path

binary = get_binary_path()  # -> "/path/to/site-packages/agentruntime_agentd/bin/agentd"
```

## Platforms

Pre-built wheels are available for:

- macOS arm64 (Apple Silicon)
- macOS x86_64 (Intel)
- Linux x86_64 (glibc and musl)
- Linux arm64 (glibc and musl)

## Links

- [agentruntime on GitHub](https://github.com/danieliser/agentruntime)
