# The binary is cross-compiled on the host (see deploy/agent/build.sh), so the
# image is just the binary on a minimal base: no shell, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
COPY bin/gpufit-agent-linux-${TARGETARCH} /gpufit-agent
ENTRYPOINT ["/gpufit-agent"]
CMD ["run"]
