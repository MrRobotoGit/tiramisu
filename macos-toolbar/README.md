# Tiramisu macOS Toolbar Monitor

A native macOS menu bar application to monitor and manage your Tiramisu instance directly from your toolbar.

## Features

- **Quick Access Dashboard**: Left-click the menu bar icon to open a sleek popover showing your Tiramisu dashboard.
- **Configurable Endpoint**: Right-click (or Ctrl-click) the icon and select **Settings** to configure your server's base URL (e.g., `http://192.168.1.2:9080`).
- **Dynamic Status Icons**:
  - 🎬 **Normal**: Server is reachable and idle.
  - 🎬 (Green background): Server is actively streaming content.
  - 🎬 (Red background): Server is unreachable or health check failed.
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
