#!/bin/sh
printf '%s %s\n' "$0" "$*" >>/canary/evidence/browser-escape
exit 97
