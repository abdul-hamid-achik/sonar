---
title: Voice
description: Listening to sonar from another room, and talking back to it.
---

Off by default. The default engine is the host synthesizer — `say` on macOS,
`espeak-ng` on Linux when it is installed — and on any system
`voice.provider: openai` speaks through a hosted engine instead, needing an
`OPENAI_API_KEY` and `ffplay` but no host synthesizer at all. Turn voice on
for the session with `/voice on`, or from the start with `voice.enabled: true`.

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

`voice.context_alert` adds one more, off by default: the context window passing
three quarters full, said once per crossing. On screen the meter already shows
it; from another room it is the difference between "still working" and "about
to compact what we said".

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

A closed set of phrases steers instead of dictating. It matches whole
utterances only, so "mostrame el diff y arreglá el bug" is dictation. Nothing
it reaches can send a prompt or cancel a turn. The full vocabulary:

| Does | Spanish | English |
| --- | --- | --- |
| Repeat the last line | otra vez · repetí · repetilo · de nuevo | again · repeat · say that again |
| Stop the audio | callate · silencio · pará | quiet · stop talking · be quiet |
| Open the diff | el diff · mostrame el diff · muéstrame el diff | diff · show the diff · show me the diff |
| Open the output | la salida · mostrame la salida | output · show the output · show me the output |
| Open the listening stage | panel · el panel | stage · show the panel |
| Go back one step | transcript · volver · atrás | back · go back |

A near-miss is dictation, deliberately: anything that is not exactly one of
these lands in the composer where you can read it before it does anything.

If an approval is waiting and **you** open the microphone, a second closed
vocabulary answers it. sonar never opens the microphone on its own. Voice can
only allow once or deny — it cannot widen a scope, and anything destructive is
refused rather than downgraded.

| Answer | Spanish | English |
| --- | --- | --- |
| Allow once | aprobalo · apruébalo | approve it · approve this |
| Deny | denegalo · rechazalo · recházalo · no lo hagas | deny · denied · reject it · don't do it |

"sí", a bare "no", "aprobado" and "approved" are absent on purpose. Approving
needs a word nobody utters by accident, because the transcriber reports no
confidence and the two directions cost different things: a wrong deny is a
refusal the model routes around, a wrong allow is a command nobody asked for.

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
/voice model gpt-4o-mini-tts   # hosted synthesis model (openai only)
/voice speak_when always|unfocused
/voice rate 195
/voice voice Samantha          # one voice for everything
/voice voice es Paulina        # one language only
/voice voice es                # forget that language's entry
/voice pronounce deploy dipló  # say it this way
/voice pronounce deploy        # say it as written
/voice input model ~/models/ggml-small.bin   # try a different Whisper size
/voice input language es|auto  # pin or re-detect the dictation language
```

Under the hosted provider, `voice.voice` names one of that provider's voices
(`nova` by default), `voice.model` picks the synthesis model, and
`voice.endpoint` — config-file only — points an OpenAI-compatible gateway at
the same request shape.

These are settings only an ear can judge, so the loop has to close in one
sitting: change it, `/voice test`, listen, change it again.

`/voice profile desk|walkaway|pair` reaches a whole mix in one command —
**desk** speaks answer and alerts only when the window loses focus,
**walkaway** adds activity and speaks always, **pair** speaks answer and
alerts always. A profile writes exactly the toggles `/voice status` reports;
it is a shortcut, not a fifth channel.

Nothing is persisted. Tuning by ear produces a state nobody should inherit by
accident on the next launch, so the session stays a session — and `/voice
status` prints the config block that would reproduce it, for when an experiment
becomes a decision.
