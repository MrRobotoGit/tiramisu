# Tiramisu macOS Toolbar Monitor

A native macOS menu bar application to monitor and manage your Tiramisu instance directly from your toolbar.

## Features

- **Quick Access Dashboard**: Left-click the menu bar icon to open a sleek popover showing your Tiramisu dashboard.
- **Configurable Endpoint**: Right-click (or Ctrl-click) the icon and select **Settings** to configure your server's address (e.g., `192.168.1.2:9080`).
- **HTTP or HTTPS, picked automatically**: when Tiramisu serves HTTPS (Control Panel › Access › HTTPS) the plain address answers `403 use https`, and the app switches to `https://` and remembers it; when HTTPS is turned off it goes back to `http://`. With the server down nothing switches. If HTTPS is on but the Mac does not trust the certificate yet, the tooltip says so: install `tiramisu-ca.crt` from the Control Panel. The API token is kept per host and port, so it survives the switch.
- **Sign-in support**: when sign-in is on (Control Panel › Access), paste an API token in **Settings**. It is stored in the macOS Keychain and sent as `Authorization: Bearer` on the status checks; the dashboard popover asks you to sign in once and keeps the session for 30 days. A missing or revoked token turns the icon red with a tooltip.
- **Dynamic Status Icon**: the Tiramisu 3×3 dot mark, drawn as a template image so it follows the light or dark menu bar.
  - **Plain**: server reachable and idle.
  - **Green background**: a stream is playing.
  - **Red background**: server unreachable or health check failed.
- **UI Optimization**: Injected JavaScript automatically hides the web dashboard header to maximize space in the menu bar popover.
- **English Interface**: All menus and settings are in English.

## Compatibility

This is a **Universal Binary** packaged as a native macOS App Bundle, running at full speed on all modern Macs:
- **Apple Silicon** (M1, M2, M3, M4, etc.)
- **Intel Processors** (x86_64)

*Requires macOS 11.0 (Big Sur) or newer.*

## Installation

1. The application is located in `Tiramisu.app`.
2. You can move `Tiramisu.app` to your `/Applications` folder for easy access.
3. Launch the application.
4. Configure your server URL via **Settings** (Right-click on the toolbar icon).
