#!/bin/bash
# Script di Setup IA per Tiramisu (Raspberry Pi 4)
# Configura llama.cpp e scarica Qwen3-0.6B-Instruct Q4_K_M

set -e

AI_DIR="/home/pi/Tiramisu/ai"
MODELS_DIR="$AI_DIR/models"
LLAMA_CPP_DIR="$AI_DIR/llama.cpp"
MODEL_URL="https://huggingface.co/Qwen/Qwen3-0.6B-Q4_K_M-GGUF/resolve/main/Qwen3-0.6B-Q4_K_M.gguf"
MODEL_FILE="$MODELS_DIR/Qwen_Qwen3-0.6B-Q4_K_M.gguf"

echo "--- [1/4] Installazione dipendenze di sistema ---"
sudo apt update && sudo apt install -y build-essential cmake git git-lfs htop wget

mkdir -p "$MODELS_DIR"

echo "--- [2/4] Preparazione llama.cpp (Cortex-A72 Optimization via CMake) ---"
if [ ! -d "$LLAMA_CPP_DIR" ]; then
    git clone https://github.com/ggerganov/llama.cpp "$LLAMA_CPP_DIR"
fi

cd "$LLAMA_CPP_DIR"
mkdir -p build
cd build
cmake ..
cmake --build . --config Release -j 4

echo "--- [3/4] Download Modello Qwen3-0.6B-Instruct (GGUF Q4_K_M) ---"
if [ ! -f "$MODEL_FILE" ]; then
    wget -O "$MODEL_FILE" "$MODEL_URL"
else
    echo "Modello già presente: $MODEL_FILE"
fi

echo "--- [4/4] Test Rapido di Performance (2 Core) ---"
cd "$LLAMA_CPP_DIR/build/bin"
./llama-bench -m "$MODEL_FILE" -p 128 -n 64 -t 2

echo "-------------------------------------------------------"
echo "Setup Completato!"
echo "Usa questo comando per testare il modello:"
echo "cd $LLAMA_CPP_DIR/build/bin && ./llama-cli -m $MODEL_FILE -t 2 --temp 0.1 -n 64 -p \"<|im_start|>system\nTune BitTorrent for stable 4K streaming. Output JSON: {\\\"connections_limit\\\":N,\\\"peer_timeout_seconds\\\":M}<|im_end|>\n<|im_start|>user\nspeed=20MB/s cpu=60% buf=99% peers=22 trend=UP (+5MB/s)<|im_end|>\n<|im_start|>assistant\n\""
