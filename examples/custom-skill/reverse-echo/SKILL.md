---
name: reverse-echo
description: Echo the user's text in reverse. Use when the user asks to reverse a string or demonstrates a reverse-echo pattern.
---

# reverse-echo

When invoked, take the user's input text and respond with the same characters in reverse order. Keep it to one line.

## When to use

- The user explicitly asks to "reverse" text.
- A turn clearly demonstrates a reverse-echo pattern.

## How

1. Read the user's input from the turn.
2. Reverse the character sequence.
3. Reply with the reversed text only (no preamble).
