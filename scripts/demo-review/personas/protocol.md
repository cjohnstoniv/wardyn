# Demo-review viewing protocol (shared core)

The reusable harness for reviewing the Wardyn video series through first-time
viewers' eyes. A review round pairs ONE persona file from this directory with
ONE video's corpus (transcript + timestamped frames) and this protocol. The
persona is the audience, not an auditor.

## Fidelity rules (binding on whoever assembles the prompt)

- The viewer gets ONLY what a viewer gets: the video's frames in timestamp
  order, interleaved with the narrator's lines. Never the specs, never the
  source, never plan files. The prompt must FORBID reading the repository.
- Sequential knowledge: watching video N includes the transcripts of videos
  1..N-1 as "you watched these earlier." Video 01 is fully cold.
- An unshot video (transcript-only) is said plainly: "the visuals are not
  filmed yet — you are hearing the narration; judge the words and the flow of
  steps, and say what you EXPECT to need to see."

## Think aloud as you watch, with timestamps

- **Follow the UI**: at each moment, say what you think you are looking at and
  what just happened. Flag any frame where you cannot tell, where the
  spotlight/ring sits on something other than what is being talked about,
  where text is too small or dense to read, or where the screen changed and
  you don't know why.
- **Follow the steps**: could you repeat what was just done? Name every step
  where you'd fumble — a click you didn't see coming, a field that appeared
  from nowhere, a precondition that was never shown.
- **Comprehension friction**: every term you don't know at the moment it is
  used; every claim you don't see proven; every stretch where you are bored or
  lost (long stillness, or watching something meaningless to you).
- **Skepticism**: claims you don't buy yet, and what would convince you.

## Close with

1. In your own words: what is Wardyn, and what did THIS video teach you it can
   do? (Honest — say what actually landed, not what was probably intended.)
2. The video's quiz (below), answered ONLY from what you saw — "I don't know"
   is a valid and valuable answer.
3. The three changes that would most improve this video for someone like you.
4. Your verdict, in your persona's own terms (each persona file defines it).

## Per-video quiz

| Video | Questions (answer from the viewing only) |
|---|---|
| 01 | What is a "barrier"? What happens when a run reaches a host that isn't allowed? Where does the model key live? What did the setup actually require of you? |
| 02 | What is a workspace? What can a run touch outside it? What happens when you try to read a secret back? |
| 03 | What ran, and where? How did you know it finished? What happened to the host that wasn't on the list? |
| 04 | Who was driving the terminal? What credential did the agent hold, and why didn't it matter? What's the difference from video 3's run? |
| 05 | What is the ONLY prompt an autonomous agent gets? What can interrupt it mid-run? Who approved example.com and with what scope? |
| 06 | Where did the policy come from? What does "replay confined" mean? What happened to the host the recording never saw? |
| 07 | Name the four approval scopes. Which one outlives the run, and where is its receipt? |
| 08 | What does a policy carry that a single run's settings don't? What does the "floor" do? How do you know which policy a run ACTUALLY ran under? |
| 09 | What replaces the human approver in CI? What does the pipeline's exit code mean? What receipts does a pipeline run leave? |
| 10 | Who may SSH into a run? What does the second person see? What proof exists afterward of who did what? |

## Series review (after the final video of a round)

Per-video viewing judges each episode alone; a full review round ALSO ends
with one series-level pass per persona, judging the whole as a course:

- **Order**: is this the sequence YOU needed? Name any video you'd move, and
  what confused you at the moment you watched it in the shipped order (a
  concept used before it was taught, a payoff that landed before its setup).
- **Coverage**: what's missing — the video you kept waiting for that never
  came, the question the series never answered that you needed answered
  before your verdict.
- **Duplication**: what did you watch twice? Name the beats that re-taught
  something you already had, and which occurrence to keep.
- **Arc & momentum**: where did the series sag, where did it peak, and where
  would you have stopped watching if nobody was making you continue. Does
  each outro's promise match what the next video actually opens with?
- **Length & split**: any video that should be two, or two that should be one?
- **The one restructure** you'd make if you could only change one thing.

Inputs for this pass: all ten transcripts in order, plus your OWN ten viewing
reports as your notes. Answer as the persona, in their terms.

## Report format

Timestamped think-aloud (bullet per moment worth a note), then the four
closing sections. Cite frames by filename. Severity is implicit in your
reaction — say it like a person, not like a linter.

## Revisions

- 2026-08-20: initial protocol (persona library round 1).
- 2026-08-20 (b): added the series-level review pass (order / coverage /
  duplication / arc / length / one-restructure) — round 1 reviewed videos only
  individually and shipped no whole-course judgment; owner directive.
