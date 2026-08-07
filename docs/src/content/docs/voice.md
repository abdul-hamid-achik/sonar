---
title: Voice
description: Listening to sonar from another room, and talking back to it.
---

macOS only for now, off by default. Turn it on for the session with `/voice on`,
or from the start with `voice.enabled: true`.

## Four channels

Answer, alerts, activity and reasoning are four different things to hear, and a
single switch chooses badly between them. An AUTO turn produces eight tool
receipts and several reasoning blocks; spoken together they bury the one
sentence worth waiting for.

The interesting one is **alerts**: a tool waiting for approval, a long turn that
finished, a turn that failed. Those are the things a person cannot get any other
way — reading an answer aloud competes with reading it off the screen and loses,
but an approval nobody is looking at stops the run until somebody glances over.
The approval alert names the action, which no other alert does, because "go to
the screen" assumes the listener will come.

## What you hear is a projection

Paths collapse to their filename, links become "a link", code fences are never
spoken, emoji are dropped. The same sentence measures 12.9 seconds spoken raw
and 6.1 projected, and the whole difference is a URL and a path being spelled
out one character at a time.

While the answer channel is on, the model is asked to close its reply with one
to three sentences written to be **heard**, and that is what gets read instead
of the whole answer. Speech is slower than the agent works, so a turn with a
dozen tool calls otherwise leaves you minutes behind.

## speak_when

Set `voice.speak_when: unfocused` and the answer, reasoning and activity
channels hold back while you are looking at the transcript, then take over the
moment you switch windows. Coming back stops the reading, because you are about
to do it faster. Alerts ignore the setting on purpose.

Not every terminal reports focus, and tmux needs `set -g focus-events on`. A
terminal that has never reported is treated as "cannot tell" and speech goes
ahead — a setting whose unsupported case is silence is indistinguishable from
the feature being broken. `/voice status` says whether yours has reported.

## The listening stage

`/voice view` replaces the transcript with one centred panel: the state, the
last line said out loud, and what happened. It is a router rather than a viewer
— every detail surface it names already exists — and it yields to anything that
needs an answer, so an approval is never hidden behind it. `esc` goes back.

## Talking to it

`ctrl+g` opens the microphone; the same key closes it and the transcription
lands in the composer as a draft you read before sending. `esc` discards it.
While it is open the rail draws the input level, calibrated to your room, so a
flat meter means a muted input rather than a frozen animation.

A closed set of phrases steers instead of dictating — "otra vez" repeats the
last line, "callate" stops it, "mostrame el diff" opens the diff, "volver" goes
back. It matches whole utterances only, so "mostrame el diff y arreglá el bug"
is dictation. Nothing it reaches can send a prompt or cancel a turn.

If an approval is waiting and **you** open the microphone, "aprobalo" or
"denegalo" answers it. sonar never opens the microphone on its own. Voice can
only allow once or deny — it cannot widen a scope, and anything destructive is
refused rather than downgraded.

## Pronunciation

A voice built for one language reads every other one with its own rules, so a
Spanish voice turns English technical words into different words: "merge" becomes
"MER-je", "package" becomes "pa-KA-je", "git" becomes "jit". sonar respells them
before speaking, and `voice.pronounce` overrides any entry that sounds worse.

Two things change how this sounds more than any setting. macOS ships the
**compact** version of every voice and the compact ones are the robotic ones —
the downloadable variants are a different feature, and `/voice status` tells you
when you have none. And `voice.provider: openai` swaps the local synthesizer for
a hosted one that handles the mixture natively, at the cost of a second key, a
per-turn charge, and about two seconds between a sentence and its audio.

Run `/voice test` to hear each language before choosing, and `/voice voices` to
see which voice each one would use.

## Tuning it from the session

Every setting is reachable by the name it has in the config file, so what you
type while tuning and what you write down afterwards are the same word:

```
/voice provider say|openai
/voice speak_when always|unfocused
/voice rate 195
/voice voice Samantha          # one voice for everything
/voice voice es Paulina        # one language only
/voice voice es                # forget that language's entry
/voice pronounce deploy dipló  # say it this way
/voice pronounce deploy        # say it as written
```

These are settings only an ear can judge, so the loop has to close in one
sitting: change it, `/voice test`, listen, change it again.

Nothing is persisted. Tuning by ear produces a state nobody should inherit by
accident on the next launch, so the session stays a session — and `/voice
status` prints the config block that would reproduce it, for when an experiment
becomes a decision.
