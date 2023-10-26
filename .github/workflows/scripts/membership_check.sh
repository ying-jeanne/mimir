#!/bin/bash

# Input variables
TOKEN="$1"
USERNAME="$2"

# Check if all input variables are provided
if [ -z "$USERNAME" ] || [ -z "$TOKEN" ]; then
  echo "Usage: $0 <GitHubUsername> <GitHubToken>"
  exit 1
fi

# Set the GitHub API URL
API_URL="https://api.github.com/orgs/grafana/teams/mimir-maintainers/members/$USERNAME"

# Send a GET request to the GitHub API
response=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: token $TOKEN" $API_URL)

# Set the result as an environment variable
if [ "$response" -eq 204 ]; then
  echo "::set-output name=team_membership::true"
elif [ "$response" -eq 404 ]; then
  echo "::set-output name=team_membership::false"
else
  echo "::set-output name=team_membership::error"
fi
