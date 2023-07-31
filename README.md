# containerd runtime v2 shim sample

A sample implementation of a [runtime v2 shim][cd-rv2] for containerd, over the [ttrpc][ttrpc] protocol.

It was built for educational purposes and is meant to emphasize the following aspects of a shim's responsibilities:

- Daemonization and re-parenting of container processes (_child subreaper_)
- Reporting of container statuses and exit codes to containerd
- Termination of container processes through OS signals
- Forwarding of container processes' _stdout_ and _stderr_ streams
- (Attaching to the _stdin_ stream of container processes)

To this end, this sample shim omits the actual _containerization_ of those processes entirely for simplicity, and
instead runs a single, hardcoded shell command on the host to help demonstrate the aforementioned aspects:

```sh
sh -c 'while date --rfc-3339=seconds; do sleep 5; done'
```

As a byproduct of its reduced scope, the shim also deliberately leaves [some RPC calls][cd-rv2-taskproto] unimplemented.

## Requirements

- A UNIX-like operating system (Linux, macOS, BSD, ...)
- [containerd][cd-release] with its companion `ctr` CLI tool
  (can also be installed using the [nerdctl-full][nctl-release] bundle)
- The [Go][go-dl] toolchain

## Usage

1. Build the shim binary for your platform and install it in your PATH, following the containerd runtime v2 [binary
   naming convention][cd-rv2-bin]:

   ```sh
   go build
   sudo install shim-sample /usr/local/bin/containerd-shim-sample-v2
   ```

1. Make sure that the containerd image store contains at least one image (any image) that you can use to create a
   container and spawn an instance of that container:

   ```sh
   sudo ctr image ls
   ```

   > **Note**  
   > As mentioned in the introduction, this sample shim implementation runs a hardcoded `sh` command on the host, and
   > therefore ignores the provided container image entirely. If your local container image store is empty, simply go
   > ahead and pull an image from your favourite container registry. I suggest Docker's official `busybox` image,
   > available on the Docker Hub:
   >
   > ```sh
   > sudo ctr image pull docker.io/library/busybox:1.36
   > ```

1. Lastly, create (but don't run) a container via containerd using the `ctr` CLI tool, explicitly using the
   `com.example.sample.v2` runtime (shim) that was built and installed in the first step:

   ```sh
   sudo ctr container create \
     --runtime com.example.sample.v2 \
     docker.io/library/busybox:1.36 \
     shim-test
   ```

   > **Note**  
   > The creation of the container is the responsibility of containerd alone. It does not involve the shim yet.

## Guided Observations

As soon as the `shim-test` container (metadata) is created, it can be listed by `ctr` using information requested from
containerd:

```console
$ sudo ctr container ls
CONTAINER    IMAGE                             RUNTIME
shim-test    docker.io/library/busybox:1.36    com.example.sample.v2
```

**Here is where the responsibility of the shim begins.**

Start a _task_ (live running process) from the container `shim-test` or, in more familiar words, _run_ the `shim-test`
container:

```console
$ sudo ctr task start --detach shim-test
(no output means success)
```

> **Note**  
> You can learn about the separation between a _Container_ and a _Task_ at [Getting started with containerd / Creating a
> running Task].

The corresponding task can be listed by `ctr` with its associated PID and status:

```console
$ sudo ctr task ls
TASK         PID     STATUS
shim-test    6014    RUNNING
```

This information matches the process tree on the host. Here is what the `ps` command returns on a Ubuntu Linux host
running inside the Windows Subsystem for Linux (WSL), where ellipses denote irrelevant information I omitted from the
command's output:

```console
$ ps axjf
 PPID   PID  PGID  ...  COMMAND
    0     1     0  ...  /init
...
    1     7     7  ...  /init
    7     8     7  ...   \_ /init
    8     9     9  ...       \_ -zsh
    9  9069  9069  ...           \_ ps axjf
...
    1  1342  1342  ...  /init
 1342  1343  1342  ...   \_ /init
 1343  6006  6006  ...       \_ /usr/local/bin/containerd-shim-sample-v2 ...
 6006  6014  6006  ...           \_ sh -c while date --rfc-3339=seconds; do sleep 5; done
 6014  9064  6006  ...               \_ sleep 5
```

> **Note**  
> Notice how the shim and its children are part of the same Process Group (PGID).

The task must be stopped by an OS signal before it can be removed:

```console
$ sudo ctr task rm shim-test
ERRO[0000] unable to delete shim-test  error="task must be stopped before deletion: ..."
```

Stopping the task with an OS signal can be achieved using the `kill` sub-command. By default, `ctr` instructs containerd
to send SIGTERM (15):

```console
$ sudo ctr task kill shim-test
(no output means success)
```

The task no longer has the status RUNNING, and transitioned to STOPPED:

```console
$ sudo ctr task ls
TASK         PID     STATUS
shim-test    6014    STOPPED
```

The shim process no longer has children processes. However, it keeps running for as long as the task still exists to
allow the status of this task to be queried, potential logs to be read, etc.:

```console
$ ps axjf
 PPID   PID  PGID  ...  COMMAND
...
    1  1342  1342  ...  /init
 1342  1343  1342  ...   \_ /init
 1343  6006  6006  ...       \_ /usr/local/bin/containerd-shim-sample-v2 ...
```

Now that the task is no longer running, it can be removed:

```console
$ sudo ctr task rm shim-test
WARN[0000] task shim-test exit with non-zero exit code 143
```

`ctr` issues a warning about the non-zero exit code of the task. This is expected, and confirms that its process was
signaled by SIGTERM (15) while issuing the `kill` sub-command (128 + 15 = 143). 

To start a new task from the same container, simply get back to the beginning of this section and replay its steps.

This time around, you could for example either start the task in _attached_ mode, or attach to a detached task with the
`attach` sub-command, and observe that it exits with the code 130 upon sending it SIGINT (2) with the key combination
`CTRL-C` (128 + 2 = 130).

[cd-rv2]: https://github.com/containerd/containerd/blob/v1.7.3/runtime/v2/README.md
[cd-rv2-bin]: https://github.com/containerd/containerd/blob/v1.7.3/runtime/v2/README.md#binary-naming
[cd-rv2-taskproto]: https://github.com/containerd/containerd/blob/v1.7.3/api/runtime/task/v2/shim.proto#L29-L51
[cd-task]: https://github.com/containerd/containerd/blob/v1.7.3/docs/getting-started.md#creating-a-running-task
[cd-release]: https://github.com/containerd/containerd/releases/tag/v1.7.3
[nctl-release]: https://github.com/containerd/nerdctl/releases/tag/v1.5.0
[ttrpc]: https://github.com/containerd/ttrpc/blob/v1.2.2/README.md
[go-dl]: https://go.dev/dl/
