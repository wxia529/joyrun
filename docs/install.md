# Installation and Upgrades

JoyRun publishes release assets through GitHub for Linux, macOS, and Windows.
The standalone installers select the matching archive and require its SHA-256
entry in the same release's `SHA256SUMS` file before replacing any executable.

## Supported release artifacts

| Platform | Architectures | Archive |
|---|---|---|
| Linux | amd64, arm64 | `.tar.gz` |
| macOS | amd64, arm64 | `.tar.gz` |
| Windows | amd64 | `.zip` |

Unsupported systems and architectures fail without modifying the installation.

## Linux and macOS

```bash
curl -fsSLO \
  https://github.com/wxia529/joyrun/releases/latest/download/install.sh
sh install.sh
```

The default destination is `~/.local/bin`. Override it without elevated
privileges:

```bash
sh install.sh --install-dir "$HOME/bin"
```

If the destination is not already in `PATH`, the installer reports the
required change but does not edit shell startup files.

## Windows

From PowerShell:

```powershell
Invoke-WebRequest `
  https://github.com/wxia529/joyrun/releases/latest/download/install.ps1 `
  -OutFile install.ps1
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\install.ps1 -AddToPath
```

The default destination is `%LOCALAPPDATA%\Programs\JoyRun`. `-AddToPath`
updates the current user's PATH, never the machine PATH. Omit it when PATH is
managed centrally. The process-scoped `ExecutionPolicy` argument lets Windows
PowerShell run the downloaded installer without changing the persisted user or
machine execution policy.

## Upgrade, pin, and inspect

An installation is also an upgrade. The candidate archive and binary are
verified before replacement, and the preceding binary is retained as
`joyrun.previous` or `joyrun.previous.exe`.

```bash
sh install.sh --check
sh install.sh --version v0.1.0
```

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\install.ps1 -Check
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\install.ps1 -Version v0.1.0
```

`--check`/`-Check` performs no filesystem writes. An omitted version means the
latest stable GitHub release; only an explicit version can select a
prerelease. Confirm installation with:

```bash
joyrun version --json
```

When the installation directory is not in PATH, verify the absolute path
instead:

```bash
"$HOME/.local/bin/joyrun" version --json
```

```powershell
& "$env:LOCALAPPDATA\Programs\JoyRun\joyrun.exe" version --json
```

## Automation policy

Automated agents may choose the platform artifact by invoking the installer;
they should not reimplement release selection. Agents must:

- use only installers and assets from `github.com/wxia529/joyrun`;
- avoid `curl | sh`, checksum bypasses, `sudo`, and machine-wide PATH changes;
- install or upgrade only when the user requested it or approved the action;
- use `--check`/`-Check` for read-only update checks;
- preserve pinned versions when reproducibility is required;
- stop on checksum, platform, version, or database-compatibility errors.

JoyRun never updates itself during `submit`, `status`, `pull`, or any other
task operation.
