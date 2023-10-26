#!/bin/bash

# Input variables
TOKEN="$1"
USERNAME="$2"

# Check if all input variables are provided
if [ -z "$USERNAME" ] || [ -z "$TOKEN" ]; then
  echo "Usage: $0 <GitHubUsername> <GitHubToken>"
  exit 1
fi

# URL-encode the USERNAME
ENCODED_USERNAME=$(printf %s "$USERNAME" | xxd -p -c 256 | tr -d '\n' | sed 's/\(..\)/%\1/g')

# Set the GitHub API URL
API_URL="https://api.github.com/orgs/grafana/teams/mimir-maintainers/members/$ENCODED_USERNAME"

# Send a GET request to the GitHub API
response=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: token $TOKEN" $API_URL)
echo "so what is the response?"
echo "$response"
# Check if the response is empty or non-integer
if [ -z "$response" ]; then
  echo "error"
else
  # Set the result as an environment variable based on the response code
  if [ "$response" -eq 204 ]; then
    echo "true"
  elif [ "$response" -eq 404 ]; then
    echo "false"
  else
    echo "error"
  fi
fi
