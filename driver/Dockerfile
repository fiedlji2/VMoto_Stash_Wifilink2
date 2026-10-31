# Sestavení ovladače 8812eu (rtl88x2eu) s CONFIG_P2P=y pro RunCam WiFiLink (SSC338Q, RubyFPV/OpenIPC)
FROM ubuntu:22.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential git wget curl ca-certificates cpio unzip rsync bc file \
      python3 python3-distutils libncurses-dev libssl-dev lzop u-boot-tools \
      automake autoconf libtool pkg-config gawk flex bison xz-utils bzip2 \
      && rm -rf /var/lib/apt/lists/*
RUN useradd -m builder
USER builder
WORKDIR /home/builder
COPY --chown=builder build.sh /home/builder/build.sh
CMD ["bash", "/home/builder/build.sh"]
