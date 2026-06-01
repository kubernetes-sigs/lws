# Copyright (c) 2024, NVIDIA CORPORATION. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Adapted from https://github.com/triton-inference-server/tutorials/blob/main/Deployment/Kubernetes/EKS_Multinode_Triton_TRTLLM/multinode_helm_chart/containers/server.py

import argparse
import os
import shutil
import signal
import subprocess
import sys
import time

ERROR_EXIT_DELAY = 15
ERROR_CODE_FATAL = 255
ERROR_CODE_USAGE = 253
EXIT_SUCCESS = 0
DELAY_BETWEEN_QUERIES = 2


def die(exit_code: int):
    if exit_code is None:
        exit_code = ERROR_CODE_FATAL

    write_error(f"       Waiting {ERROR_EXIT_DELAY} second before exiting.")
    # Delay the process' termination to provide a small window for administrators to capture the logs before it exits and restarts.
    time.sleep(ERROR_EXIT_DELAY)

    exit(exit_code)


def parse_arguments():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", type=str, choices=["leader", "worker"])
    parser.add_argument(
        "--triton_model_repo_dir",
        type=str,
        default=None,
        required=True,
        help="Directory that contains Triton Model Repo to be served",
    )
    parser.add_argument("--pp", type=int, default=1, help="Pipeline parallelism.")
    parser.add_argument("--tp", type=int, default=1, help="Tensor parallelism.")
    parser.add_argument("--iso8601", action="count", default=0)
    parser.add_argument("--verbose", action="store_true", help="Enable verbose output")
    parser.add_argument(
        "--namespace",
        type=str,
        default="default",
        help="Namespace of the Kubernetes deployment.",
    )
    parser.add_argument(
        "--gpu_per_node",
        type=int,
        help="How many gpus are in each pod/node (We launch one pod per node). Only required in leader mode.",
    )
    parser.add_argument(
        "--stateful_set_group_key",
        type=str,
        default=None,
        help="Value of leaderworkerset.sigs.k8s.io/group-key, Leader uses this to gang schedule and its only needed in leader mode",
    )
    parser.add_argument(
        "--enable_nsys", action="store_true", help="Enable Triton server profiling"
    )

    return parser.parse_args()


def run_command(cmd_args: [str], omit_args: [int] = None):
    command = ""

    for i, arg in enumerate(cmd_args):
        command += " "
        if omit_args is not None and i in omit_args:
            command += "*****"
        else:
            command += arg

    write_output(f">{command}")
    write_output(" ")

    return subprocess.call(cmd_args, stderr=sys.stderr, stdout=sys.stdout)


def signal_handler(sig, frame):
    write_output(f"Signal {sig} detected, quitting.")
    exit(EXIT_SUCCESS)


def wait_for_workers(num_total_pod: int, args):
    if num_total_pod is None or num_total_pod <= 0:
        raise RuntimeError("Argument `world_size` must be greater than zero.")

    write_output("Begin waiting for worker pods.")

    cmd_args = [
        "kubectl",
        "get",
        "pods",
        "-n",
        f"{args.namespace}",
        "-l",
        f"leaderworkerset.sigs.k8s.io/group-key={args.stateful_set_group_key}",
        "--field-selector",
        "status.phase=Running",
        "-o",
        "jsonpath='{.items[*].metadata.name}'",
    ]
    command = " ".join(cmd_args)

    workers = []

    while len(workers) < num_total_pod:
        time.sleep(DELAY_BETWEEN_QUERIES)

        if args.verbose:
            write_output(f"> {command}")

        output = subprocess.check_output(cmd_args).decode("utf-8")

        if args.verbose:
            write_output(output)

        output = output.strip("'")

        workers = output.split(" ")

        if len(workers) < num_total_pod:
            write_output(
                f"Waiting for worker pods, {len(workers)} of {num_total_pod} ready."
            )
        else:
            write_output(f"{len(workers)} of {num_total_pod} workers ready.")

    write_output(" ")

    if workers is not None and len(workers) > 1:
        workers.sort()

    return workers


def write_output(message: str):
    print(message, file=sys.stdout, flush=True)


def write_error(message: str):
    print(message, file=sys.stderr, flush=True)


def do_leader(args):
    write_output(
        f"Server is assuming each node has {args.gpu_per_node} GPUs. To change this, use --gpu_per_node"
    )

    world_size = args.tp * args.pp

    if world_size <= 0:
        raise Exception(
            "usage: Options --tp and --pp must both be equal to or greater than 1."
        )

    write_output(f"Executing Leader (world size: {world_size})")

    workers = wait_for_workers(world_size / args.gpu_per_node, args)

    if len(workers) != (world_size / args.gpu_per_node):
        write_error(
            f"fatal: {len(workers)} found, expected {world_size / args.gpu_per_node}."
        )
        die(ERROR_EXIT_DELAY)

    workers_with_mpi_slots = [worker + f":{args.gpu_per_node}" for worker in workers]

    if args.enable_nsys:
        cmd_args = [
            "/var/run/models/nsight-systems-cli-DVS/bin/nsys",
            "profile",
            "--force-overwrite",
            "true",
            "-t",
            "cuda,nvtx",
            "--enable",
            "efa_metrics",
            "-o",
            "/var/run/models/nsys_report",
            "/opt/hpcx/ompi/bin/mpirun",
            "--allow-run-as-root",
        ]
    else:
        cmd_args = [
            "/opt/hpcx/ompi/bin/mpirun",
            "--allow-run-as-root",
        ]

    if args.verbose:
        cmd_args += ["--debug-devel"]

    cmd_args += [
        "--report-bindings",
        "-mca",
        "plm_rsh_agent",
        "kubessh",
        "-np",
        f"{world_size}",
        "--host",
        ",".join(workers_with_mpi_slots),
    ]

    # Add per node command lines separated by ':'.
    for i in range(world_size):
        if i != 0:
            cmd_args += [":"]

        cmd_args += [
            "-n",
            "1",
            "tritonserver",
            "--allow-cpu-metrics=false",
            "--allow-gpu-metrics=false",
            "--disable-auto-complete-config",
            f"--id=rank{i}",
            "--model-load-thread-count=2",
            f"--model-repository={args.triton_model_repo_dir}",
        ]

        # Rank0 node needs to support metrics collection and web services.
        if i == 0:
            cmd_args += [
                "--allow-metrics=true",
                "--metrics-interval-ms=1000",
            ]

            if args.verbose:
                cmd_args += ["--log-verbose=2"]

            if args.iso8601 > 0:
                cmd_args += ["--log-format=ISO8601"]

        # Rank(N) nodes can disable metrics, web services, and logging.
        else:
            cmd_args += [
                "--allow-http=false",
                "--allow-grpc=false",
                "--allow-metrics=false",
                "--model-control-mode=explicit",
                "--load-model=tensorrt_llm",
                "--log-info=false",
                "--log-warning=false",
            ]

    result = run_command(cmd_args)

    if result != 0:
        die(result)

    exit(result)


def do_worker(args):
    signal.signal(signal.SIGINT, signal_handler)
    signal.signal(signal.SIGTERM, signal_handler)

    write_output("Worker paused awaiting SIGINT or SIGTERM.")
    signal.pause()


def main():
    write_output("Reporting system information.")
    run_command(["whoami"])
    run_command(
        ["cgget", "-n", "--values-only", "--variable memory.limit_in_bytes", "/"]
    )
    run_command(["nvidia-smi"])

    args = parse_arguments()
    if args.triton_model_repo_dir is None:
        raise Exception(f"--triton_model_repo_dir is required")

    if args.verbose:
        write_output(f"Triton model repository is at:'{args.triton_model_repo_dir}'")

    if args.mode == "leader":
        if args.gpu_per_node is None:
            raise Exception("--gpu_per_node is required for leader mode")
        if args.stateful_set_group_key is None:
            raise Exception("--stateful_set_group_key is required for leader mode")
        do_leader(args)
    elif args.mode == "worker":
        do_worker(args)
    else:
        write_error(f"usage: server.py <mode> [<options>].")
        write_error(f'       Invalid mode ("{args.mode}") provided.')
        write_error(f'       Supported values are "init" or "exec".')
        die(ERROR_CODE_USAGE)


if __name__ == "__main__":
    main()
