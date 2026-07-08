"""
idle.py -- a deliberately slow task for the lifecycle reaper scenario.
The agent is asked to run this; it sleeps in a loop so the run stays RUNNING
long enough for the operator to observe the auto-stop timer fire.
"""
import time
import sys

def main():
    print("idle.py: starting 10-minute idle loop", flush=True)
    for i in range(120):
        print(f"idle.py: tick {i+1}/120 (sleeping 5 s)", flush=True)
        time.sleep(5)
    print("idle.py: loop complete", flush=True)

if __name__ == "__main__":
    main()
