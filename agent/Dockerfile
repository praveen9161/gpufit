# The binary is cross-compiled on the host (see deploy/agent/build.sh), so the
# image is just the binary on a minimal base: no shell, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
COPY bin/gpufit-agent-linux-${TARGETARCH} /gpufit-agent
COPY bin/gpufit-controller-linux-${TARGETARCH} /gpufit-controller
# Default: the agent. The controller Deployment sets command: [/gpufit-controller].
ENTRYPOINT ["/gpufit-agent"]
CMD ["run"]
