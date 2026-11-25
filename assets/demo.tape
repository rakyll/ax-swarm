# Demo screencast for the README. Records a real ax-swarm run (real AX
# Gateway/Workspace/Tasks and a real agy /swarm session) against the local AX
# cluster. Regenerate with:
#
#   vhs assets/demo.tape
#
# Requires ax-swarm, ax, and agy on PATH and a running AX cluster.

Output assets/demo.gif

Require ax-swarm
Require agy
Require ax

Set Shell "bash"
Set FontSize 15
Set Width 1400
Set Height 820
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set Framerate 15
# A real run takes a few minutes; play it back 6x faster.
Set PlaybackSpeed 6

Hide
Type "export PATH=$HOME/go/bin:$HOME/.local/bin:$PATH PS1='$ ' && clear"
Enter
Show

Type `ax-swarm --name swarm-demo "Write a Python palindrome checker with two unit tests in the workspace and run the tests"`
Sleep 500ms
Enter

# Wait for agy to finish, ax-swarm to clean up, and the shell prompt to return.
Wait+Line@20m /^\$\s*$/
Sleep 4s
