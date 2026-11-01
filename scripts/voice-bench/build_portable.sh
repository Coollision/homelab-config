#!/bin/sh
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq && apt-get install -y -qq git cmake g++ make curl ca-certificates libvulkan-dev glslc spirv-headers libgomp1 >/dev/null 2>&1 || { echo APT-FAIL; exit 1; }
[ -d /src ] || git clone -q --depth 1 https://github.com/ggml-org/whisper.cpp /src || exit 1
cd /src
cmake -B build -DCMAKE_BUILD_TYPE=Release -DGGML_NATIVE=OFF -DGGML_AVX=OFF -DGGML_AVX2=OFF -DGGML_AVX512=OFF -DGGML_FMA=OFF -DGGML_F16C=OFF -DGGML_BMI2=OFF -DGGML_SSE42=ON -DGGML_VULKAN=ON -DBUILD_SHARED_LIBS=ON -DWHISPER_BUILD_EXAMPLES=ON > /tmp/cmake.log 2>&1 || { echo CMAKE-FAIL; tail -5 /tmp/cmake.log; exit 1; }
cmake --build build -j3 --target whisper-server > /tmp/build.log 2>&1 || { echo BUILD-FAIL; tail -5 /tmp/build.log; exit 1; }
mkdir -p /out/lib && cp build/bin/whisper-server /out/ && cp build/src/libwhisper.so* build/ggml/src/libggml*.so* build/ggml/src/ggml-vulkan/libggml-vulkan.so* /out/lib/ 2>/dev/null; cp /usr/lib/x86_64-linux-gnu/libgomp.so.1 /out/lib/
cd /out && tar czf /tmp/portable.tgz . && echo BUILD-OK
