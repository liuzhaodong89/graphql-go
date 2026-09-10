#!/bin/sh
# Copy to tokens.sh (gitignored), fill in, then:  . ./tokens.sh
#
# Do NOT paste tokens into a chat transcript, a commit, or a config file.
# Do NOT `export TOK=value` directly on the command line either: it lands in
# shell history and in the terminal scrollback.
#
# To fill this in without echoing, use an editor, or read it interactively:
#   read -rs SHOPIFY_STOREFRONT_TOKEN && export SHOPIFY_STOREFRONT_TOKEN
#   read -rs SHOPLINE_STOREFRONT_TOKEN && export SHOPLINE_STOREFRONT_TOKEN

export SHOPIFY_STOREFRONT_TOKEN=""
export SHOPLINE_STOREFRONT_TOKEN=""

# Login fixtures for S1, if that scenario is enabled. Same rule: not in git.
export CMPBENCH_TEST_ACCOUNT_PASSWORD=""
