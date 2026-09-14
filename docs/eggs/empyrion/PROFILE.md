# Empyrion: the Windows profile

The install script in this directory is only half the egg. The other half is the
Windows profile, set in the Panel plugin's egg editor, and Empyrion is the egg
that needs the most of it — it is the case the console settings were built for.

## What the Linux egg does

Its startup line is not a command. It is a shell pipeline:

```sh
cd DedicatedServer ; WINEDLLOVERRIDES="steamclient64=,winedbg.exe=d" WINEDEBUG=-all \
  wine EmpyrionDedicated.exe -batchmode -nographics -logFile ../Logs/server/server.log & PID=$! ;
tail -c0 -F ../Logs/server/server.log &
until nc -z 127.0.0.1 21004; do sleep 1; done ;
telnet -E 127.0.0.1 21004 ;
wait $PID
```

Five separate things, only one of which starts a server:

1. run the game in the background,
2. tail its log into the container's stdout, because the game writes nothing to
   stdout itself,
3. wait for the console port to open,
4. attach a telnet client to it with the container's stdin on the other end,
   because the game reads nothing from stdin either,
5. `wait $PID` so that the *game*, not the pipeline, decides when the server has
   stopped.

Steps 2 to 5 are not the egg's business here. The node does them itself, against
the game process it already supervises, and the profile is how it is told to.
Wine and the DLL overrides go away entirely: the Windows depot the Linux egg
forces with `WINDOWS_INSTALL=1` is simply the native build on this host.

## The profile

| Field | Value |
|---|---|
| Runtime | *(none — the game is self-contained)* |
| Working directory | `DedicatedServer` |
| Startup command | `EmpyrionDedicated.exe -batchmode -nographics -logFile ..\Logs\server\server.log` |
| Stop | Command → `saveandexit 0` |
| Pseudo console | off |
| Console output | Follow a log file → `Logs/server/server.log` |
| Console commands | TCP console → port `{{TELNET_PORT}}`, password `{{TELNET_PASSWORD}}` |

### Why the working directory is `DedicatedServer`

The Linux line begins `cd DedicatedServer`, and everything after it is relative
to that — which is why the log path carries a `../`. It is tempting to read the
`cd` as scaffolding and drop it, rewriting the paths to suit a server started in
its data directory. That works right up until the game starts a second process.

Empyrion runs each playfield in a child of its own, and it builds that child's
command line itself:

```
Started process 'EmpyrionPlayfieldServer.exe' (PID 7960) with args:
  -batchmode -nographics -logFile ../Logs/5150/Playfield_260910-213152-40.log
```

That `../` is the game's, not the egg's, and nothing in the profile can reach it.
Started from the data directory it resolves to the server's *root* — the daemon's
half of the tree, which the server's account is denied and must stay denied,
since `worker.json` lives there. The playfield process dies on an
`UnauthorizedAccessException` it reports as a failure to create a Logs
directory, on a directory whose permissions look perfectly correct because it is
a different Logs entirely.

So the `cd` is reproduced rather than removed: a working directory of
`DedicatedServer` puts the game where it expects to be, and every `../` it
computes for itself lands back inside the data directory. The startup command
keeps its own `..\Logs\server\server.log` for the same reason, which makes this
profile a near-transliteration of the Linux one.

**Console output does not follow the working directory.** Its path stays relative
to the data directory, so it is `Logs/server/server.log` with no prefix while the
startup command's is `..\Logs\server\server.log`. Two different bases for what is
the same file, which looks like a mistake and is not: the daemon reads that log
itself, and pinning it to the data directory means moving the process does not
silently move where the daemon looks.

Getting either wrong is quiet in a way worth knowing about — Unity opens whatever
path it is given and carries on, so a server with a mistyped `-logFile` runs
perfectly well with a console that never says anything.

The install script creates `Logs\server\` because Unity opens that path but will
not create the directories leading to it. The playfield directories underneath —
one per port — the game creates itself, once it is started somewhere it is
allowed to.

### Why the stop is a command

Empyrion has no console to interrupt — it is `-batchmode -nographics` and its
only command interface is the telnet port. A signal stop would have nothing to
deliver, so the node would kill it and the world would lose whatever had not been
saved. `saveandexit 0` goes over the command channel, which is exactly what the
telnet client in the Linux egg was there to let a human do.

### Why there is no connect timeout set

The node waits five minutes by default for the console port to open, which is a
world-loading time rather than a network timeout. A large save on a slow disk can
exceed that; raise it in the profile if the console never connects but the server
is plainly running.

## What it looks like when it is working

The server console shows the log file from the moment the game starts writing it,
then Empyrion's telnet banner once the port opens — that banner is how you tell a
connected channel from a silent one. Commands typed into the Panel go to the
port, and their replies come back onto the console underneath them.

If the console shows the log but no banner, the port is wrong: check
`{{TELNET_PORT}}` against `TelnetPort` in `dedicated.yaml`, which the Panel
substitutes at boot. The node says so in its log as well, and a command typed
before the channel connects is answered on the console rather than silently
dropped.
