#!/usr/bin/env bash
# Session A now runs through the shared runner: ../run.sh session-a
exec "$(dirname "$0")/../run.sh" session-a
