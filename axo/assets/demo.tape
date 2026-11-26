# Demo screencast for the axo README. Records a real axo run (one real AX
# Gateway and Workspace, real AX Tasks driven by plain agy -p workers)
# against the local AX cluster. Regenerate from the repository root with:
#
#   (cd axo && make install) && vhs axo/assets/demo.tape
#
# The raw recording of this multi-minute run is large (~18 MB), so shrink it
# for the README afterwards (2x faster, 5 fps, 1120 px wide, 64 colors):
#
#   ffmpeg -i axo/assets/demo.gif -vf "setpts=0.5*PTS,fps=5,scale=1120:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=64[p];[b][p]paletteuse=dither=none:diff_mode=rectangle" /tmp/demo.gif && mv /tmp/demo.gif axo/assets/demo.gif
#
# Requires axo, ax, and agy on PATH and a running AX cluster.

Output axo/assets/demo.gif

Require axo
Require agy
Require ax

Set Shell "bash"
Set FontSize 20
Set Width 1400
Set Height 820
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms
Set Framerate 15
# A real run takes a few minutes; play it back 8x faster.
Set PlaybackSpeed 8

# Workers run `ax ssh`; use the ax on PATH, which matches the cluster's AX
# version, rather than the default $AX_ROOT/bin/ax. The distinctive prompt
# lets the Wait below detect when axo has exited; an empty PS2 keeps the
# continuation line of the two-line command clean.
Hide
Type "export PATH=$HOME/go/bin:$HOME/.local/bin:$PATH AX_BIN=$(command -v ax) PS1='❯ ' PS2='' && clear"
Enter
Show

Type `axo --workspace '{"setup":{"git":[{"repo":"https://github.com/chalk/chalk"}]}}' \`
Enter
Type `  "Why would I use this library, and what are its weak spots?"`
Sleep 500ms
Enter

# Wait for axo to synthesize the answer, clean up, and return the prompt.
Wait+Line@20m /^❯\s*$/
Sleep 4s
