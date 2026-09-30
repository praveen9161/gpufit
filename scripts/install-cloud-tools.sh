#!/usr/bin/env bash
# Guided install of the tools for the real-GPU session. Shows every command.
#
#   ./scripts/install-cloud-tools.sh
set -euo pipefail
cd "$(dirname "$0")"
source ./guided-lib.sh

step 1 "What's already here:" \
       "  AWS CLI: installed (used later with a SEPARATE 'gpufit' profile, not 'default')." \
       "  Terraform: not installed."
run "aws --version"
run "which terraform || echo 'terraform: not installed'"
pause

step 2 "Install Terraform from HashiCorp's own Homebrew tap." \
       "(Homebrew's main repo no longer ships new Terraform versions since the license change," \
       " so HashiCorp publishes it in their tap: hashicorp/tap.)"
run "brew tap hashicorp/tap"
run "brew install hashicorp/tap/terraform"
pause

step 3 "Check it works."
run "terraform version"
run "which terraform"

printf '\n\033[1;36m━━ DONE ━━\033[0m\n'
echo "Nothing touches AWS yet. Next: create the AWS account, then set up the 'gpufit' profile."
