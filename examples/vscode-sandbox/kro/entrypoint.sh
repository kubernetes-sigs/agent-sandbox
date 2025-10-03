#!/bin/bash


export GEMINI_API_KEY=`cat /tokens/gemini`

set -x

# protection against running gemini on an unpause
# if gemini-promt.txt dont run gemini else create and run it
if [ -f gemini-prompt.txt ]; then
  echo "gemini-prompt.txt exists, skipping gemini generation"
else
  echo "gemini-prompt.txt does not exist, running gemini"
  echo "$AGENT_PROMPT" > gemini-prompt.txt
  gemini -y  -p  "$AGENT_PROMPT" > gemini-output.txt || true
fi

/usr/local/bin/code-server-entrypoint
