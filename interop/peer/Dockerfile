# Copyright 2026 Abhinav Gautam
# SPDX-License-Identifier: Apache-2.0
#
# Reference IEC 60870-5 stations for the interop tests: lib60870-C v2.4.1
# (GPLv3, github.com/mz-automation/lib60870) with mbedtls 3.6.6 for TLS,
# built from source, the stock cs104_server and cs104_client examples, and
# peer.c of this directory linked against them. socat bridges a pty to TCP
# for IEC 101. The image is a test peer only; nothing of it is distributed.
#
#   docker build -t sigma-iec60870-interop:lib60870-2.4.1 .

FROM debian:bookworm-slim AS build
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc g++ libc6-dev make cmake git ca-certificates curl bzip2 patch \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch v2.4.1 https://github.com/mz-automation/lib60870 /src/lib60870
WORKDIR /src/lib60870/lib60870-C/dependencies
RUN curl -fsSL -o mbedtls.tar.bz2 https://github.com/Mbed-TLS/mbedtls/releases/download/mbedtls-3.6.6/mbedtls-3.6.6.tar.bz2 \
 && tar xjf mbedtls.tar.bz2 && rm mbedtls.tar.bz2
WORKDIR /src/lib60870/lib60870-C
RUN cmake -S . -B /build -DCMAKE_BUILD_TYPE=Release -DBUILD_TESTS=OFF \
 && cmake --build /build -j4 \
 && cmake --install /build --prefix /usr/local
COPY peer.c /peer/
RUN gcc -O2 -o /usr/local/bin/peer /peer/peer.c -I/usr/local/include -I/usr/local/include/lib60870 \
    -L/usr/local/lib -llib60870 -lpthread \
 && cp /build/examples/cs104_server/cs104_server /usr/local/bin/cs104_server \
 && cp /build/examples/cs104_client/simple_client /usr/local/bin/cs104_client

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends socat \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /usr/local/bin/peer /usr/local/bin/cs104_server /usr/local/bin/cs104_client /usr/local/bin/
COPY --from=build /usr/local/lib/ /usr/local/lib/
COPY serial101.sh /usr/local/bin/serial101.sh
RUN ldconfig
