#!/usr/bin/env bash
if [ -z "$1" ] || [ "$1" == "--help" ] || [ "$1" == "-h" ]; then
    echo "curl-graph.sh is a dev tool useful for exploring the Microsoft Graph API via curl."
    echo
    echo "$(tput bold)Usage:$(tput sgr0)   ./curl-graph.sh [auth-token-file] api_endpoint [other curl options]"
    echo "$(tput bold)Example:$(tput sgr0) ./curl-graph.sh ~/.cache/onedriver/home-user-OneDrive/auth_tokens.json /me"
    exit 0
fi

if [ -f "$1" ]; then
    TOKEN=$(jq -r .access_token "$1")
    ENDPOINT="$2"
    shift 2
else
    # tokens are stored per mount: use the first mount's
    TOKEN=$(jq -r .access_token "$(ls ~/.cache/onedriver/*/auth_tokens.json | head -1)")
    ENDPOINT="$1"
    shift 1
fi 

curl -s -H "Authorization: bearer $TOKEN" $@ "https://graph.microsoft.com/v1.0$ENDPOINT" | jq .
