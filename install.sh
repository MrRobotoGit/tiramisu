#!/usr/bin/env bash
# ==============================================================================
# Tiramisu Installer
# Tiramisu + GoStorm (Unified Engine)
# Target: auto-detected at install time
# ==============================================================================
set -e

# ------------------------------------------------------------------------------
# Color output — standard 8-color + 256-color palette
# ------------------------------------------------------------------------------
if [ -t 1 ] && command -v tput >/dev/null 2>&1; then
    RED=$(tput setaf 1)
    GREEN=$(tput setaf 2)
    YELLOW=$(tput setaf 3)
    BLUE=$(tput setaf 4)
    BOLD=$(tput bold)
    NC=$(tput sgr0)
    _ncolors=$(tput colors 2>/dev/null || echo 8)
else
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    BLUE='\033[0;34m'
    BOLD='\033[1m'
    NC='\033[0m'
    _ncolors=8
fi

# 256-color palette (ANSI 38;5;N — falls back to standard colors)
if [ "${_ncolors:-8}" -ge 256 ] 2>/dev/null; then
    P1=$'\e[38;5;27m'    # deep blue       (logo line 1)
    P2=$'\e[38;5;33m'    # blue            (logo line 2)
    P3=$'\e[38;5;39m'    # blue-cyan       (logo line 3)
    P4=$'\e[38;5;45m'    # cyan            (logo line 4)
    P5=$'\e[38;5;51m'    # bright cyan     (logo line 5/6)
    PDIM=$'\e[38;5;240m' # dark gray       (decorative lines)
    PSUB=$'\e[38;5;246m' # medium gray     (subtitle / payoff)
    PGRN=$'\e[38;5;82m'  # bright green    (ok icon)
    PYLW=$'\e[38;5;220m' # amber           (warn icon)
    PRED=$'\e[38;5;196m' # bright red      (err icon)
    PTEAL=$'\e[38;5;45m' # teal            (header accent)
    PBOX=$'\e[38;5;33m'  # blue            (summary box)
    PRST=$'\e[0m'
else
    P1="$BLUE"; P2="$BLUE"; P3="$BLUE"; P4="$BLUE"; P5="$BLUE"
    PDIM="$NC"; PSUB="$NC"; PGRN="$GREEN"; PYLW="$YELLOW"; PRED="$RED"
    PTEAL="$BLUE"; PBOX="$BLUE"; PRST="$NC"
fi

# ------------------------------------------------------------------------------
# Terminal width helpers
# ------------------------------------------------------------------------------
get_cols() { tput cols 2>/dev/null || echo 80; }

# Full-width horizontal rule: print_hr [color] [char]
print_hr() {
    local col="${1:-$PDIM}" char="${2:-─}"
    local w; w=$(get_cols)
    printf "%s  " "$col"
    local _i; for ((_i=0; _i<w-4; _i++)); do printf "%s" "$char"; done
    printf "%s\n" "$PRST"
}

# Centered wizard form: width of the centered block and its left padding
FORM_W=64
form_pad() {
    local cols; cols=$(get_cols)
    local p=$(( (cols - FORM_W) / 2 ))
    echo $(( p < 2 ? 2 : p ))
}

# ------------------------------------------------------------------------------
# Funny messages — one per phase category
# ------------------------------------------------------------------------------
_MSGS_SETUP=(
    "OMG am I doing this for real?! Say goodbye to my grades."
    "Am I ditching all the other services? Sorry Netflix, it's not me, it's definitely you."
    "Look at me saving money for more beers... I mean, 'soda'. Obviously."
)
_MSGS_WORK=(
    "Loading... honestly, I'm just as bored as you are right now."
    "Wait, I'm actually working? Someone call my mom, she won't believe it."
    "Don't mind me, just downloading your next 3 a.m. obsession."
)
_MSGS_DONE=(
    "Aaaand done. Now go rot on the couch like the legend you are."
    "We're in. If anyone asks, you're 'studying'. I got your back."
    "Setup finished. Go ahead, ignore those 47 unread texts. You deserve this."
)

# Pick a random element from a named array (bash 3.2 compatible — no nameref)
pick_msg() {
    local _arr="$1"
    local _len; eval "_len=\${#${_arr}[@]}"
    [ "$_len" -eq 0 ] && return
    local _idx=$(( RANDOM % _len ))
    eval "echo \"\${${_arr}[$_idx]}\""
}

# ------------------------------------------------------------------------------
# Progress bar for [3/3] Install phase
# ------------------------------------------------------------------------------
_STEP=0
_STEPS=9  # clone, deploy, config, dirs, services, enable, sudoers, compile, verify

step_start() {
    (( _STEP++ )) || true
    local label="$1"
    local cols; cols=$(get_cols)
    local msg; msg=$(pick_msg _MSGS_WORK)

    tput clear 2>/dev/null || printf '\033[2J\033[H'
    draw_logo

    local bar_w=$(( cols - 16 ))
    local _i

    # Phase bar — always 3/3, pre-filled to 2/3
    local ph_filled=$(( bar_w * 2 / 3 ))
    local ph_empty=$(( bar_w - ph_filled ))
    printf "  %s" "$P3"
    for ((_i=0; _i<ph_filled; _i++)); do printf "█"; done
    printf "%s" "$PDIM"
    for ((_i=0; _i<ph_empty; _i++)); do printf "░"; done
    printf "%s  %sPhase 3/3%s\n" "$PRST" "$PSUB" "$PRST"
    printf "  %s%s◆  [3/3] Installing%s%s\n\n" "$PTEAL$BOLD" "$PRST" "$NC$PRST" ""

    # Step bar — secondary (▓ fill char to distinguish from phase bar)
    local st_filled=$(( bar_w * _STEP / _STEPS ))
    local st_empty=$(( bar_w - st_filled ))
    printf "  %s" "$P4"
    for ((_i=0; _i<st_filled; _i++)); do printf "▓"; done
    printf "%s" "$PDIM"
    for ((_i=0; _i<st_empty; _i++)); do printf "░"; done
    printf "%s  %s%d/%d%s\n" "$PRST" "$PSUB" "$_STEP" "$_STEPS" "$PRST"
    echo ""
    printf "  %s▸  %s%s%s\n" "$PTEAL" "$BOLD" "$label" "$NC$PRST"
    echo ""
    printf "  %s%s%s\n" "$PSUB" "$msg" "$PRST"
    print_hr "$PDIM"
    echo ""
}

# ------------------------------------------------------------------------------
# draw_logo — full 6-line TIRAMISU ASCII logo (centered, used on splash + summary)
# ------------------------------------------------------------------------------
draw_logo() {
    local cols; cols=$(get_cols)
    local pad=$(( (cols - 72) / 2 ))
    [ "$pad" -lt 0 ] && pad=0
    local sp; printf -v sp '%*s' "$pad" ''

    echo ""
    printf "%s%s████████╗ ██╗ ██████╗   █████╗  ███╗   ███╗ ██╗ ███████╗ ██╗   ██╗ %s\n"  "$P2" "$sp" "$PRST"
    printf "%s%s╚══██╔══╝ ██║ ██╔══██╗ ██╔══██╗ ████╗ ████║ ██║ ██╔════╝ ██║   ██║ %s\n" "$P3" "$sp" "$PRST"
    printf "%s%s   ██║    ██║ ██████╔╝ ███████║ ██╔████╔██║ ██║ ███████╗ ██║   ██║ %s\n"  "$P4" "$sp" "$PRST"
    printf "%s%s   ██║    ██║ ██╔══██╗ ██╔══██║ ██║╚██╔╝██║ ██║ ╚════██║ ██║   ██║ %s\n" "$P4" "$sp" "$PRST"
    printf "%s%s   ██║    ██║ ██║  ██║ ██║  ██║ ██║ ╚═╝ ██║ ██║ ███████║ ╚██████╔╝ %s\n" "$P3" "$sp" "$PRST"
    printf "%s%s   ╚═╝    ╚═╝ ╚═╝  ╚═╝ ╚═╝  ╚═╝ ╚═╝     ╚═╝ ╚═╝ ╚══════╝  ╚═════╝  %s\n" "$P2" "$sp" "$PRST"
    echo ""
    local payoff="✦  The Torrent Service  ✦"
    local ppad=$(( (cols - ${#payoff}) / 2 ))
    [ "$ppad" -lt 0 ] && ppad=0
    printf "%*s%s%s%s\n" "$ppad" "" "$PSUB" "$payoff" "$PRST"
    echo ""
    echo ""
}

# ------------------------------------------------------------------------------
# draw_header — compact 1-line brand strip for wizard/install screens
# ------------------------------------------------------------------------------
draw_header() {
    local cols; cols=$(get_cols)
    local brand="  T I R A M I S U  ✦  The Torrent Service · Installer"
    local bp=$(( (cols - ${#brand}) / 2 ))
    [ "$bp" -lt 0 ] && bp=0
    printf "\n%*s%s%s%s%s\n" "$bp" "" "$P4$BOLD" "$brand" "$NC$PRST"
    print_hr "$PDIM"
}

# ------------------------------------------------------------------------------
# wizard_header — clears screen, draws compact header + phase progress bar
# Usage: wizard_header "Phase Title" current_step total_steps
# ------------------------------------------------------------------------------
wizard_header() {
    local title="$1" step="${2:-1}" total="${3:-3}"
    local cols; cols=$(get_cols)
    local msg
    [ "$step" -lt "$total" ] && msg=$(pick_msg _MSGS_SETUP) || msg=$(pick_msg _MSGS_WORK)

    tput clear 2>/dev/null || printf '\033[2J\033[H'
    draw_logo

    local bar_w=$(( cols - 16 ))
    local filled=$(( bar_w * step / total ))
    local empty=$(( bar_w - filled ))
    printf "  %s" "$P3"
    local _i; for ((_i=0; _i<filled; _i++)); do printf "█"; done
    printf "%s" "$PDIM"
    for ((_i=0; _i<empty; _i++)); do printf "░"; done
    printf "%s  %sPhase %d/%d%s\n" "$PRST" "$PSUB" "$step" "$total" "$PRST"
    echo ""
    printf "  %s%s◆  %s%s\n" "$PTEAL" "$BOLD" "$title" "$NC$PRST"
    echo ""
    printf "  %s%s%s\n" "$PSUB" "$msg" "$PRST"
    print_hr "$PDIM"
    echo ""
}

# ------------------------------------------------------------------------------
# Helper: print a colored section header (full-width separator, no clear)
# ------------------------------------------------------------------------------
print_header() {
    echo ""
    printf "  %s%s◆  %s%s%s\n" "$PTEAL" "$BOLD" "$1" "$NC" "$PRST"
    print_hr "$PDIM"
    echo ""
}

print_ok()   { printf "  %s✔%s  %s\n" "$PGRN"  "$PRST" "$1"; }
print_warn() { printf "  %s⚠%s   %s\n" "$PYLW"  "$PRST" "$1"; }
print_err()  { printf "  %s✘%s  %s\n" "$PRED"  "$PRST" "$1"; }
print_info() { printf "  %s→%s  %s\n" "$PTEAL" "$PRST" "$1"; }

# ------------------------------------------------------------------------------
# Helper: ask "Prompt" "default" VAR_NAME
#   Displays [default] hint; if user presses Enter, uses default.
# ------------------------------------------------------------------------------
ask() {
    local prompt="$1" default="$2" var_name="$3" user_input
    local pad; pad=$(form_pad)
    local sp; printf -v sp '%*s' "$pad" ''

    echo ""
    printf "%s%s%s%s\n" "$sp" "$BOLD" "$prompt" "$NC"
    if [ -n "$default" ]; then
        printf "%s%sDefault:%s %s\n" "$sp" "$PSUB" "$PRST" "$default"
    fi
    printf "%s%s❯%s " "$sp" "$P4" "$PRST"
    read -r user_input
    [ -z "$user_input" ] && user_input="$default"
    printf -v "$var_name" '%s' "$user_input"
}

# ------------------------------------------------------------------------------
# Helper: ask_secret "Prompt" VAR_NAME
#   Hidden input (no echo); no default shown for security.
# ------------------------------------------------------------------------------
ask_secret() {
    local prompt="$1" var_name="$2" user_input
    local pad; pad=$(form_pad)
    local sp; printf -v sp '%*s' "$pad" ''

    echo ""
    printf "%s%s%s%s\n" "$sp" "$BOLD" "$prompt" "$NC"
    printf "%s%s(hidden input)%s\n" "$sp" "$PSUB" "$PRST"
    printf "%s%s❯%s " "$sp" "$P4" "$PRST"
    read -rs user_input
    echo ""
    printf -v "$var_name" '%s' "$user_input"
}

# ------------------------------------------------------------------------------
# Helper: ask_yn "Question" [default_yn]
#   Returns 0 for yes, 1 for no.
#   default_yn should be "y" or "n" (case-insensitive).
# ------------------------------------------------------------------------------
ask_yn() {
    local question="$1" default="${2:-n}" hint user_input
    local pad; pad=$(form_pad)
    local sp; printf -v sp '%*s' "$pad" ''

    if [ "${default,,}" = "y" ]; then
        hint="${PGRN}${BOLD}Yes${NC}${PRST}  ${PDIM}/  No${PRST}  ${PSUB}(recommended: Yes)${PRST}"
    else
        hint="${PDIM}Yes  /${PRST}  ${PRED}${BOLD}No${NC}${PRST}  ${PSUB}(recommended: No)${PRST}"
    fi

    echo ""
    printf "%s%s%s%s\n" "$sp" "$BOLD" "$question" "$NC"
    printf "%s%s%s\n" "$sp" "$hint" "$PRST"
    printf "%s%s❯%s " "$sp" "$P4" "$PRST"
    read -r user_input
    [ -z "$user_input" ] && user_input="$default"

    case "${user_input,,}" in
        y|yes) return 0 ;;
        *)     return 1 ;;
    esac
}

# ------------------------------------------------------------------------------
# show_banner — full-screen splash (clears terminal, waits for Enter)
# ------------------------------------------------------------------------------
show_banner() {
    local cols; cols=$(get_cols)
    tput clear 2>/dev/null || printf '\033[2J\033[H'

    draw_logo

    echo ""
    print_hr "$PDIM"
    printf "  %sPlatform%s  $(uname -m) / $(uname -s)   %s│%s  Samba (optional) · built-in scheduler · FUSE\n" \
        "$PTEAL" "$PRST" "$PDIM" "$PRST"
    print_hr "$PDIM"
    echo ""

    local hint="  Press Enter to begin installation  "
    local hpad=$(( (cols - ${#hint}) / 2 ))
    [ "$hpad" -lt 0 ] && hpad=0
    printf "%*s%s%s%s\n\n" "$hpad" "" "$PSUB" "$hint" "$PRST"
    read -r
}

# ==============================================================================
# [0a] Auto-install system dependencies via apt
# ==============================================================================
install_system_deps() {
    # Enable user_allow_other in /etc/fuse.conf (required for FUSE allow_other mount option).
    # Runs unconditionally, independent of whether any apt packages need installing —
    # a system that already has fuse3 installed (e.g. Raspberry Pi OS, or a re-run of
    # this installer) would otherwise silently skip this and fail the FUSE mount later.
    if [ -f /etc/fuse.conf ]; then
        if ! grep -q "^user_allow_other" /etc/fuse.conf; then
            sudo sed -i 's/^#\s*user_allow_other/user_allow_other/' /etc/fuse.conf
            grep -q "^user_allow_other" /etc/fuse.conf || echo "user_allow_other" | sudo tee -a /etc/fuse.conf >/dev/null
            print_ok "FUSE: user_allow_other enabled in /etc/fuse.conf"
        fi
    fi

    # Only run on Debian/Ubuntu-based systems
    if ! command -v apt-get >/dev/null 2>&1; then
        print_warn "apt-get not found — skipping automatic dependency installation."
        print_warn "Please install manually: git fuse3 curl"
        if [ "${INSTALL_SAMBA:-true}" = "true" ]; then
            print_warn "Samba: samba"
        fi
        return 0
    fi

    # Map: package name → apt package to install
    local -A needed=()

    command -v git         >/dev/null 2>&1 || needed["git"]="git"
    command -v fusermount3 >/dev/null 2>&1 || needed["fusermount3"]="fuse3"
    command -v curl        >/dev/null 2>&1 || needed["curl"]="curl"
    # Samba is optional — only check if user wants it
    if [ "${INSTALL_SAMBA:-true}" = "true" ]; then
        dpkg -s samba        >/dev/null 2>&1 || needed["samba"]="samba"
    fi
    # libfuse3-dev is required for CGO_ENABLED=1 compilation (provides fuse.h)
    dpkg -s libfuse3-dev   >/dev/null 2>&1 || needed["libfuse3-dev"]="libfuse3-dev"
    # gcc is required for CGO
    command -v gcc         >/dev/null 2>&1 || needed["gcc"]="gcc"

    if [ "${#needed[@]}" -eq 0 ]; then
        print_ok "All system dependencies already installed."
        return 0
    fi

    print_header "Installing System Dependencies"
    print_info "Missing packages: ${needed[*]}"
    print_info "Running: sudo apt-get update && sudo apt-get install -y ${needed[*]}"
    echo ""

    sudo apt-get update -qq
    sudo apt-get install -y "${needed[@]}"

    echo ""
    print_ok "System dependencies installed."
}

# ==============================================================================
# [0] Prerequisite checks
# ==============================================================================
check_prerequisites() {
    print_header "Checking Prerequisites"

    local all_ok=true

    # fusermount3 or fusermount (FUSE userspace tool)
    if command -v fusermount3 >/dev/null 2>&1; then
        print_ok "fusermount3"
        FUSERMOUNT_CMD="fusermount3"
    elif command -v fusermount >/dev/null 2>&1; then
        print_ok "fusermount (fusermount3 preferred but fusermount found)"
        FUSERMOUNT_CMD="fusermount"
    else
        print_err "fusermount3/fusermount not found (install: sudo apt install fuse3)"
        all_ok=false
    fi

    # systemctl
    if command -v systemctl >/dev/null 2>&1; then
        print_ok "systemctl"
    else
        print_err "systemctl not found (systemd required for service management)"
        all_ok=false
    fi

    # curl (used for Plex auto-discovery and health-check — not strictly fatal)
    if command -v curl >/dev/null 2>&1; then
        print_ok "curl"
    else
        print_warn "curl not found — Plex library auto-discovery will be skipped"
    fi

    echo ""
    if [ "$all_ok" = false ]; then
        print_err "One or more required prerequisites are missing. Please install them and re-run."
        exit 1
    fi
}

# ==============================================================================
# [1/3] System Paths + User/Group
# ==============================================================================
collect_paths() {
    wizard_header "[1/3] System Paths" 1 3

    # Default: Tiramisu subdirectory next to the installer
    local default_install_dir="${SCRIPT_DIR}/Tiramisu"
    local default_user
    default_user=$(whoami)
    local default_group
    default_group=$(id -gn "$default_user" 2>/dev/null || echo "$default_user")

    ask "Tiramisu install directory" "$default_install_dir" INSTALL_DIR
    ask "Physical MKV source path   (physical_source_path)" "/mnt/tiramisu-mkv-real" STORAGE_PATH
    ask "FUSE virtual mount path     (fuse_mount_path)"     "/mnt/tiramisu-mkv-virtual" FUSE_MOUNT
    ask "System user that owns Tiramisu" "$default_user" SYSTEM_USER
    ask "System group" "$default_group" SYSTEM_GROUP

    # Resolve to absolute path
    INSTALL_DIR="$(cd "$(dirname "${INSTALL_DIR}")" 2>/dev/null && pwd)/$(basename "${INSTALL_DIR}")" || INSTALL_DIR="$(pwd)/${INSTALL_DIR}"
    mkdir -p "${INSTALL_DIR}"

    # Derive BASE_DIR as INSTALL_DIR
    BASE_DIR="${INSTALL_DIR}"

    # Centered confirmation summary
    local pad; pad=$(form_pad)
    local sp; printf -v sp '%*s' "$pad" ''
    echo ""
    printf "%s%s✔  Paths confirmed%s\n" "$sp" "$PGRN" "$PRST"
    printf "%s%s   Install dir   :%s %s\n" "$sp" "$PDIM" "$PRST" "$INSTALL_DIR"
    printf "%s%s   Source path   :%s %s\n" "$sp" "$PDIM" "$PRST" "$STORAGE_PATH"
    printf "%s%s   FUSE mount    :%s %s\n" "$sp" "$PDIM" "$PRST" "$FUSE_MOUNT"
}

# ==============================================================================
# [2/3] Samba (optional)
# ==============================================================================
collect_options() {
    wizard_header "[2/3] Options" 2 3

    INSTALL_SAMBA=true
    local pad; pad=$(form_pad)
    local sp; printf -v sp '%*s' "$pad" ''
    if ask_yn "Install and configure Samba?" "y"; then
        INSTALL_SAMBA=true
        echo ""
        printf "%s%s✔  Samba will be installed and configured.%s\n" "$sp" "$PGRN" "$PRST"
    else
        INSTALL_SAMBA=false
        echo ""
        printf "%s%s→  Samba skipped — configure an alternative access method post-install.%s\n" "$sp" "$PSUB" "$PRST"
    fi
}

# ==============================================================================
# [5/5] Installing — step implementations
# ==============================================================================

# ------------------------------------------------------------------------------
# 5a. Generate config.json from config.json.example
# ------------------------------------------------------------------------------
generate_config_json() {
    step_start "Generate config.json"
    local output_path="${INSTALL_DIR}/config.json"
    local example_path="${INSTALL_DIR}/config.json.example"

    if [ ! -f "$example_path" ]; then
        print_err "config.json.example not found — cannot generate config."
        exit 1
    fi

    # Copy the example and patch only the path fields with sed
    cp "$example_path" "$output_path"
    sed -i "s|\"physical_source_path\": \".*\"|\"physical_source_path\": \"${STORAGE_PATH}\"|" "$output_path"
    sed -i "s|\"fuse_mount_path\": \".*\"|\"fuse_mount_path\": \"${FUSE_MOUNT}\"|" "$output_path"

    print_ok "config.json written to ${output_path}"
    print_info "Configure Plex, TMDB, NAT-PMP, ports, and scheduler from the Control Panel at :9080/control"
}

# ------------------------------------------------------------------------------
# 5a. Clone or use existing repo source
# ------------------------------------------------------------------------------
clone_repo() {
    step_start "Clone / copy source"

    # Check if this is already a cloned repo (main.go exists in SCRIPT_DIR)
    if [ -f "${SCRIPT_DIR}/main.go" ]; then
        # If INSTALL_DIR is inside SCRIPT_DIR (e.g. SCRIPT_DIR=/home/pi, INSTALL_DIR=/home/pi/Tiramisu),
        # don't rsync the entire parent directory — clone fresh instead
        case "${INSTALL_DIR}" in
            "${SCRIPT_DIR}"/*)
                print_info "Install directory is inside source directory — cloning fresh from GitHub..."
                ;;
            *)
                if [ "$(realpath "${SCRIPT_DIR}")" != "$(realpath "${INSTALL_DIR}")" ]; then
                    print_info "Copying source to ${INSTALL_DIR}..."
                    rsync -a "${SCRIPT_DIR}/" "${INSTALL_DIR}/" --exclude='.git'
                    print_ok "Source copied to ${INSTALL_DIR}"
                    return 0
                fi
                print_ok "Source found in ${SCRIPT_DIR} — using existing clone."
                return 0
                ;;
        esac
    fi

    # Clone from GitHub
    local repo_url="https://github.com/MrRobotoGit/tiramisu.git"
    local tmp_clone="/tmp/tiramisu-clone-$$"

    print_info "Cloning source from GitHub..."
    if command -v git >/dev/null 2>&1; then
        git clone --depth 1 "$repo_url" "$tmp_clone"
        mkdir -p "${INSTALL_DIR}"
        rsync -a "${tmp_clone}/" "${INSTALL_DIR}/"
        rm -rf "$tmp_clone"

        # Remove committed Go module cache (causes go mod tidy errors)
        if [ -d "${INSTALL_DIR}/go/pkg/mod" ]; then
            rm -rf "${INSTALL_DIR}/go/pkg/mod"
        fi

        print_ok "Source cloned to ${INSTALL_DIR}"
    else
        print_err "git not found — cannot clone source."
        exit 1
    fi
}

# ------------------------------------------------------------------------------
# 5a2. Deploy config.json.example
# ------------------------------------------------------------------------------
deploy_files() {
    step_start "Deploy config.json.example"
    print_info "Deploying files to ${INSTALL_DIR}..."

    # Ensure INSTALL_DIR exists
    mkdir -p "${INSTALL_DIR}"

    # config.json.example should already be in INSTALL_DIR from clone_repo
    if [ -f "${INSTALL_DIR}/config.json.example" ]; then
        print_ok "config.json.example present in ${INSTALL_DIR}/"
    else
        print_info "config.json.example not found — downloading from GitHub..."
        local raw_url="https://raw.githubusercontent.com/MrRobotoGit/tiramisu/refs/heads/main/config.json.example"
        if curl -sfL -o "${INSTALL_DIR}/config.json.example" "$raw_url"; then
            print_ok "config.json.example downloaded from GitHub"
        else
            print_err "Failed to download config.json.example from GitHub."
            exit 1
        fi
    fi
}

# ------------------------------------------------------------------------------
# 5b. Create directories
# ------------------------------------------------------------------------------
create_directories() {
    step_start "Create directories"

    # Directories inside INSTALL_DIR (user-writable)
    local local_dirs=(
        "${INSTALL_DIR}/STATE"
        "${INSTALL_DIR}/logs"
    )

    for d in "${local_dirs[@]}"; do
        mkdir -p "$d"
        print_ok "Created: $d"
    done

    # Data directories (MKV source + FUSE mount point — may need sudo)
    local data_dirs=(
        "${STORAGE_PATH}/movies"
        "${STORAGE_PATH}/tv"
        "${FUSE_MOUNT}"
    )

    for d in "${data_dirs[@]}"; do
        if mkdir -p "$d" 2>/dev/null; then
            chown "${SYSTEM_USER}:${SYSTEM_GROUP}" "$d" 2>/dev/null || true
            print_ok "Created: $d"
        else
            print_info "Creating ${d} requires sudo..."
            sudo mkdir -p "$d"
            sudo chown "${SYSTEM_USER}:${SYSTEM_GROUP}" "$d"
            print_ok "Created (sudo): $d"
        fi
    done
}

# ------------------------------------------------------------------------------
# 5d. Install systemd service files
# ------------------------------------------------------------------------------
install_services() {
    step_start "Install systemd service"

    local samba_restart=""
    if [ "$INSTALL_SAMBA" = "true" ]; then
        samba_restart='
# Allow tiramisu to stabilize, then restart Samba so it sees the FUSE mount
ExecStartPost=/bin/sleep 2
ExecStartPost=/usr/bin/sudo /bin/systemctl restart smbd'
    else
        samba_restart='
# Allow tiramisu to stabilize
ExecStartPost=/bin/sleep 2'
    fi

    sudo tee /etc/systemd/system/tiramisu.service > /dev/null <<SERVICE_EOF
[Unit]
Description=Tiramisu + GoStorm (Unified Streaming Engine)
After=network-online.target systemd-resolved.service nss-lookup.target local-fs.target remote-fs.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
# Memory tuning — GOMEMLIMIT=2200MiB is optimal for Pi 4 / 4GB
Environment=GOMEMLIMIT=2200MiB
Environment=GOGC=100

Type=simple
User=${SYSTEM_USER}
Group=${SYSTEM_GROUP}

WorkingDirectory=${INSTALL_DIR}

# Wait for DNS before starting (required for tracker + blocklist resolution)
ExecStartPre=/bin/sh -c 'for i in 1 2 3 4 5; do getent hosts google.com >/dev/null 2>&1 && exit 0 || sleep 2; done; exit 1'

# FUSE mount cleanup and creation
ExecStartPre=-/usr/bin/${FUSERMOUNT_CMD} -uz ${FUSE_MOUNT}
ExecStartPre=/bin/mkdir -p ${FUSE_MOUNT}

# V1.4.6: Main binary — using --path . for true portability (STATE stays in WorkingDirectory)
ExecStart=${INSTALL_DIR}/tiramisu --path .${samba_restart}

Restart=always
RestartSec=10
LimitNOFILE=65536
LimitNPROC=4096

# Centralized logging inside the Tiramisu directory (relative to WorkingDirectory)
StandardOutput=append:logs/tiramisu.log
StandardError=append:logs/tiramisu.log

# Cleanly unmount FUSE on stop
ExecStop=/usr/bin/${FUSERMOUNT_CMD} -uz ${FUSE_MOUNT}

[Install]
WantedBy=multi-user.target
SERVICE_EOF

    print_ok "Wrote /etc/systemd/system/tiramisu.service"
}

# ------------------------------------------------------------------------------
# 5e. Enable services via systemd
# ------------------------------------------------------------------------------
enable_services() {
    step_start "Enable service"
    print_info "Reloading systemd and enabling services..."

    sudo systemctl daemon-reload
    sudo systemctl enable tiramisu

    print_ok "Services enabled: tiramisu"
}

# ------------------------------------------------------------------------------
# 5g. Sudoers entry so tiramisu.service can restart smbd without a password
# ------------------------------------------------------------------------------
GO_BIN=""
GO_ARCH=""
GO_OS=""

detect_go_arch() {
    local machine
    machine="$(uname -m)"
    GO_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"

    case "$machine" in
        aarch64|arm64)    GO_ARCH="arm64"   ;;
        x86_64|amd64)     GO_ARCH="amd64"   ;;
        armv7l|armv7)     GO_ARCH="arm"     ;;
        armv6l|armv6)     GO_ARCH="armv6l"  ;;
        i686|i386)        GO_ARCH="386"     ;;
        *)
            print_err "Unsupported architecture: ${machine}"
            exit 1
            ;;
    esac

    print_info "Detected platform: ${GO_OS}/${GO_ARCH}"
}

ensure_go() {
    local go_install_dir="/usr/local/go"

    detect_go_arch

    # Find an existing Go binary that matches the detected OS/arch
    local candidates=("${go_install_dir}/bin/go" "$(command -v go 2>/dev/null)")
    for candidate in "${candidates[@]}"; do
        if [ -x "$candidate" ]; then
            local info
            info=$("$candidate" version 2>/dev/null)
            if echo "$info" | grep -q "${GO_OS}/${GO_ARCH}"; then
                GO_BIN="$candidate"
                print_ok "Go found: $info"
                return 0
            fi
        fi
    done

    # Fetch the latest stable Go version number from go.dev
    local go_version
    go_version=$(curl -fsSL "https://go.dev/VERSION?m=text" | head -1)
    if [ -z "$go_version" ]; then
        go_version="go1.24.0"   # fallback if network unavailable
    fi

    print_info "${go_version} (${GO_OS}/${GO_ARCH}) not found — downloading..."

    local tarball="${go_version}.${GO_OS}-${GO_ARCH}.tar.gz"
    local url="https://go.dev/dl/${tarball}"
    local tmp="/tmp/${tarball}"

    curl -fL --progress-bar -o "$tmp" "$url"
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$tmp"
    rm -f "$tmp"

    GO_BIN="${go_install_dir}/bin/go"
    print_ok "Go installed: $($GO_BIN version)"
}

# ------------------------------------------------------------------------------
# 5f3. Compile the Tiramisu binary from source
# ------------------------------------------------------------------------------
ensure_swap() {
    # Go compilation can OOM on Pi with little/no swap — ensure at least 1GB
    local swap_total
    swap_total=$(free -m | awk '/^Swap:/ {print $2}')
    if [ "${swap_total:-0}" -lt 1024 ]; then
        print_info "Swap < 1 GB detected (${swap_total} MB) — creating 1 GB swapfile for compilation..."
        if [ ! -f /swapfile ]; then
            sudo fallocate -l 1G /swapfile 2>/dev/null || sudo dd if=/dev/zero of=/swapfile bs=1M count=1024 status=none
            sudo chmod 600 /swapfile
            sudo mkswap /swapfile >/dev/null
        fi
        sudo swapon /swapfile 2>/dev/null || true
        print_ok "Swapfile active ($(free -m | awk '/^Swap:/ {print $2}') MB total swap)"
    fi
}

compile_binary() {
    step_start "Compile binary  (takes a few minutes on Pi 4)"
    ensure_go
    ensure_swap

    local src_dir="${INSTALL_DIR}"
    local out_bin="${INSTALL_DIR}/tiramisu"

    # Verify we have Go source files in the expected location
    if [ ! -f "${src_dir}/main.go" ]; then
        print_err "main.go not found in ${src_dir} — cannot compile."
        exit 1
    fi

    cd "${src_dir}"

    # Clean Go module cache if committed (causes go mod tidy errors with @version paths)
    if [ -d "${src_dir}/go/pkg/mod" ]; then
        rm -rf "${src_dir}/go/pkg/mod"
    fi

    print_info "Running go mod tidy..."
    GOTOOLCHAIN=local "$GO_BIN" mod tidy

    # Use -pgo=off if no default.pgo present (fresh install)
    local pgo_flag="-pgo=off"
    if [ -f "${src_dir}/default.pgo" ]; then
        pgo_flag="-pgo=auto"
        print_info "PGO profile found — building with -pgo=auto"
    else
        print_info "No PGO profile — building with -pgo=off (regenerate later for 5-7% CPU gain)"
    fi

    # -p 2 limits parallel jobs to keep peak RAM under control on Pi 4
    # GOTMPDIR on the main FS avoids OOM on small /tmp tmpfs during linking
    local go_tmp="${HOME}/go-tmp"
    mkdir -p "${go_tmp}"

    # Embed version: try local git tag first, then GitHub API, then version.go
    local app_version
    app_version=$(git describe --tags --abbrev=0 2>/dev/null || true)

    if [ -z "$app_version" ] && command -v curl >/dev/null 2>&1; then
        app_version=$(curl -fsSL --max-time 5 \
            "https://api.github.com/repos/MrRobotoGit/tiramisu/releases/latest" \
            2>/dev/null | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
    fi

    local ldflags=""
    if [ -n "$app_version" ]; then
        ldflags="-X main.AppVersion=${app_version}"
        print_info "Embedding version: ${app_version}"
    else
        print_info "No version tag found — using hardcoded version from version.go"
    fi

    print_info "Building binary (GOARCH=${GO_ARCH}, -p 2)..."

    # Spinner during go build (runs in background, main shell polls)
    _spinner_pid=""
    _spinner() {
        local frames=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')
        local msgs=(
            "Compiling... grab a coffee, you deserve it."
            "Still going... the Pi is doing its best, okay?"
            "Linking... almost there. Probably."
            "Building... this is why they invented coffee breaks."
        )
        local i=0 m=0 tick=0
        while true; do
            local frame="${frames[$(( i % ${#frames[@]} ))]}"
            local msg="${msgs[$(( m % ${#msgs[@]} ))]}"
            printf "\r  %s%s%s  %s%s%s" "$P4" "$frame" "$PRST" "$PSUB" "$msg" "$PRST"
            sleep 0.1
            (( i++ )) || true
            (( tick++ )) || true
            # rotate message every ~4 seconds (40 ticks)
            if (( tick % 40 == 0 )); then (( m++ )) || true; fi
        done
    }
    _spinner &
    _spinner_pid=$!

    local _build_exit=0
    GOTOOLCHAIN=local GOARCH="${GO_ARCH}" CGO_ENABLED=1 GOTMPDIR="${go_tmp}" \
        "$GO_BIN" build ${pgo_flag} -p 2 ${ldflags:+-ldflags "${ldflags}"} -o "${out_bin}" . \
        2>/tmp/tiramisu_build.log || _build_exit=$?

    # Kill spinner and clear the spinner line
    kill "$_spinner_pid" 2>/dev/null; wait "$_spinner_pid" 2>/dev/null || true
    printf "\r%*s\r" "$(get_cols)" ""   # erase spinner line

    if [ "$_build_exit" -ne 0 ]; then
        print_err "Build failed. Log:"
        cat /tmp/tiramisu_build.log >&2
        rm -f /tmp/tiramisu_build.log
        exit "$_build_exit"
    fi
    rm -f /tmp/tiramisu_build.log
    rm -rf "${go_tmp}"

    chmod +x "${out_bin}"
    print_ok "Binary compiled and deployed: ${out_bin}"

    cd - >/dev/null
}

# ------------------------------------------------------------------------------
# 5h. Verify installation (non-fatal — binary may not be deployed yet)
# ------------------------------------------------------------------------------
verify_install() {
    step_start "Verify installation"

    local url="http://127.0.0.1:9080/metrics"
    if command -v curl >/dev/null 2>&1; then
        if curl -sf --max-time 5 "$url" >/dev/null 2>&1; then
            print_ok "Tiramisu metrics endpoint is reachable at ${url}"
        else
            print_warn "Tiramisu is not running yet (metrics endpoint not reachable)."
            print_warn "This is expected — start with: sudo systemctl start tiramisu"
        fi
    else
        print_warn "curl not available — skipping endpoint verification."
    fi

    # 100% step bar + brief pause before summary screen
    local cols; cols=$(get_cols)
    local bar_w=$(( cols - 16 ))
    echo ""
    printf "  %s" "$PGRN"
    local _i; for ((_i=0; _i<bar_w; _i++)); do printf "▓"; done
    printf "%s  %s%s9/9  100%%%s\n" "$PRST" "$PGRN" "$BOLD" "$PRST$NC"
    echo ""
    local done_hint="  All done! Loading summary...  "
    local dp=$(( (cols - ${#done_hint}) / 2 ))
    [ "$dp" -lt 0 ] && dp=0
    printf "%*s%s%s%s\n" "$dp" "" "$PSUB" "$done_hint" "$PRST"
    sleep 1
}

# ------------------------------------------------------------------------------
# Sudoers entry so tiramisu.service can restart smbd without a password
# ------------------------------------------------------------------------------
setup_sudoers() {
    step_start "Configure sudoers"
    if [ "$INSTALL_SAMBA" != "true" ]; then
        print_info "Samba not installed — skipping sudoers entry."
        return 0
    fi

    print_info "Configuring sudoers for smbd restart..."

    local sudoers_file="/etc/sudoers.d/tiramisu-smbd"
    local sudoers_line="${SYSTEM_USER} ALL=(ALL) NOPASSWD: /bin/systemctl restart smbd"

    # Check whether the entry already exists anywhere in sudoers
    if sudo grep -qF "$sudoers_line" /etc/sudoers /etc/sudoers.d/* 2>/dev/null; then
        print_ok "Sudoers entry already present — no change needed."
        return 0
    fi

    if sudo sh -c "echo '${sudoers_line}' | tee ${sudoers_file} > /dev/null"; then
        sudo chmod 440 "${sudoers_file}"
        print_ok "Sudoers entry written: ${sudoers_file}"
    else
        print_warn "Could not write sudoers entry (sudo unavailable or permission denied)."
        print_warn "To add manually, run:"
        print_warn "  echo '${sudoers_line}' | sudo tee ${sudoers_file} && sudo chmod 440 ${sudoers_file}"
    fi
}

# ==============================================================================
# Final summary — clears screen, shows full logo + completion screen
# ==============================================================================
show_summary() {
    local cols; cols=$(get_cols)
    local msg; msg=$(pick_msg _MSGS_DONE)

    tput clear 2>/dev/null || printf '\033[2J\033[H'
    draw_logo

    echo ""
    print_hr "$PGRN" "═"
    printf "  %s%s  ✔  Installation Complete!%s\n" "$PGRN" "$BOLD" "$PRST$NC"
    printf "  %s  %s%s\n" "$PSUB" "$msg" "$PRST"
    print_hr "$PGRN" "═"
    echo ""

    printf "  %sFiles written:%s\n" "$PTEAL" "$PRST"
    printf "    %s%s/config.json%s\n" "$BOLD" "$INSTALL_DIR" "$NC"
    printf "    %s/etc/systemd/system/tiramisu.service%s\n" "$BOLD" "$NC"
    echo ""

    if [ "$INSTALL_SAMBA" = "true" ]; then
        printf "  %s⚠  Samba — edit /etc/samba/smb.conf:%s\n" "$PYLW" "$PRST"
        printf "  %s   oplocks = no  │  aio read size = 1  │  deadtime = 15  │  vfs objects = fileid%s\n" "$PDIM" "$PRST"
        printf "  %s   Then: sudo systemctl restart smbd%s\n" "$PYLW" "$PRST"
        echo ""
    fi

    print_hr "$PDIM"
    printf "  %s%sNext steps%s\n" "$BOLD" "$PTEAL" "$PRST$NC"
    print_hr "$PDIM"
    echo ""

    printf "  %s 1 %s  sudo systemctl start tiramisu\n" "$PBOX$BOLD" "$PRST$NC"
    printf "  %s 2 %s  sudo systemctl status tiramisu\n" "$PBOX$BOLD" "$PRST$NC"
    printf "  %s 3 %s  tail -f %s/logs/tiramisu.log\n" "$PBOX$BOLD" "$PRST$NC" "$INSTALL_DIR"
    echo ""

    # Card background — dark gray (256-color only, transparent on 8-color)
    local BG=""
    [ "${_ncolors:-8}" -ge 256 ] && BG=$'\e[48;5;235m'

    local bi=$(( cols - 6 ))   # box inner width (chars between ║ and ║)
    local _i

    # Draw top border with title embedded: 1(═) + title_len + rest(═) = bi ✓
    _btop() {
        local t="$1" tl=${#1}
        local rest=$(( bi - tl - 1 ))
        [ $rest -lt 0 ] && rest=0
        printf "  %s%s╔═%s%s" "$PBOX" "$BOLD" "$t" "$PBOX$BOLD"
        for ((_i=0; _i<rest; _i++)); do printf "═"; done
        printf "╗%s\n" "$PRST$NC"
    }

    # Draw a centered text line inside the box: lpad + text_len + rpad = bi ✓
    _bline() {
        local t="$1" fc="${2:-$PRST}"
        local tl=${#t}
        local lp=$(( (bi - tl) / 2 ))
        local rp=$(( bi - lp - tl ))
        [ $lp -lt 0 ] && lp=0
        [ $rp -lt 0 ] && rp=0
        printf "  %s║%s" "$PBOX$BOLD" "$BG"
        printf "%${lp}s" ""
        printf "%s%s%s" "$fc" "$t" "$PRST"
        printf "%s%${rp}s" "$BG" ""
        printf "%s║%s\n" "$PRST$PBOX$BOLD" "$PRST$NC"
    }

    # Draw a blank line inside the box
    _bblank() {
        printf "  %s║%s%${bi}s%s║%s\n" \
            "$PBOX$BOLD" "$BG" "" "$PRST$PBOX$BOLD" "$PRST$NC"
    }

    # Draw bottom border
    _bbot() {
        printf "  %s%s╚" "$PBOX" "$BOLD"
        for ((_i=0; _i<bi; _i++)); do printf "═"; done
        printf "╝%s\n" "$PRST$NC"
    }

    printf "\n"

    # ── Control Panel ────────────────────────────────────────────────────────
    _btop "  Control Panel  "
    _bline "Plex · TMDB · NAT-PMP · Ports · Scheduler · Webhooks" "$PSUB"
    _bblank
    _bline "http://<your-ip>:9080/control" "$BOLD$P4"
    _bbot

    echo ""

    # ── Dashboard ────────────────────────────────────────────────────────────
    _btop "  Dashboard  "
    _bblank
    _bline "http://<your-ip>:9080/dashboard" "$BOLD$P4"
    _bbot

    echo ""
}

# ==============================================================================
# Main
# ==============================================================================
main() {
    show_banner

    # Directory containing this script (= cloned repo root)
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

    # Note: FUSERMOUNT_CMD is set inside check_prerequisites
    FUSERMOUNT_CMD="fusermount3"

    # Default: Samba enabled (can be overridden by collect_options)
    INSTALL_SAMBA=true

    collect_paths
    collect_options

    install_system_deps
    check_prerequisites

    wizard_header "[3/3] Installing" 3 3

    clone_repo
    echo ""
    deploy_files
    echo ""
    generate_config_json
    echo ""
    create_directories
    echo ""
    install_services
    echo ""
    enable_services
    echo ""
    setup_sudoers
    echo ""
    compile_binary
    echo ""
    verify_install

    show_summary
}

main "$@"