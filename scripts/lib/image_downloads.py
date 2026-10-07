#!/usr/bin/env python3
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
"""Bounded Dockerfile download check; unsupported shell syntax fails closed."""

import json
import posixpath
import re
import shlex
import sys
from dataclasses import dataclass, field
from pathlib import Path


def instructions(text):
    pending, start = "", 0
    for number, line in enumerate(text.splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if not pending:
            start = number
        continued = line.endswith("\\")
        pending += line[:-1] if continued else line
        if not continued:
            parts = pending.strip().split(None, 1)
            name, body = parts[0], parts[1] if len(parts) == 2 else ""
            yield start, name.upper(), body.strip()
            pending = ""
    if pending:
        raise ValueError(f"unfinished Dockerfile continuation at line {start}")


@dataclass
class Token:
    value: str
    operator: bool = False
    raw: str = ""


def word_end(text, index, closing=""):
    quote = ""
    while index < len(text):
        char = text[index]
        if char == "\\" and quote != "'":
            index += 2
            continue
        if text.startswith("$(", index) and quote != "'":
            index = word_end(text, index + 2, ")") + 1
            continue
        if char == "`" and quote != "'":
            raise ValueError("backtick substitutions are unsupported; use a separate checked download")
        if char == quote:
            quote = ""
        elif not quote:
            if char in "\"'":
                quote = char
            elif closing and char == closing:
                return index
            elif closing and char == "(":
                index = word_end(text, index + 1, ")")
            elif not closing and (char.isspace() or char in ";&|()<>"):
                return index
        index += 1
    if quote or closing:
        raise ValueError("unterminated shell quote or substitution")
    return index


def tokenize(text):
    result, index = [], 0
    while index < len(text):
        if text[index].isspace():
            index += 1
            continue
        if text[index] == "#":
            break
        operator = re.match(r"&&|\|\||;;|>>|<<|>&|<&|[;&|()<>]", text[index:])
        if operator:
            value = operator[0]
            result.append(Token(value, True, value))
            index += len(value)
            continue
        end = word_end(text, index)
        raw = text[index:end]
        values = shlex.split(raw)
        if len(values) != 1:
            raise ValueError(f"unsupported shell word: {raw}")
        result.append(Token(values[0], False, raw))
        index = end
    return result


@dataclass
class Node:
    kind: str
    words: list = field(default_factory=list)
    children: list = field(default_factory=list)
    ops: list = field(default_factory=list)


class Parser:
    def __init__(self, tokens):
        self.tokens = tokens
        self.index = 0

    def at(self, *values):
        if self.index == len(self.tokens):
            return False
        token = self.tokens[self.index]
        return token.value in values and (token.operator or token.raw == token.value)

    def take(self, expected=None):
        if self.index == len(self.tokens) or (expected and not self.at(expected)):
            raise ValueError(f"unsupported shell syntax; expected {expected or 'a word'}")
        token = self.tokens[self.index]
        self.index += 1
        return token

    def sequence(self, *stops):
        result = []
        while self.index < len(self.tokens) and not self.at(*stops):
            terms, ops = [self.pipeline()], []
            while self.at("&&", "||"):
                ops.append(self.take().value)
                terms.append(self.pipeline())
            result.append(Node("chain", children=terms, ops=ops))
            if self.at(";"):
                self.take()
            elif self.index < len(self.tokens) and not self.at(*stops):
                raise ValueError(f"unsupported shell separator: {self.take().value}")
        return result

    def pipeline(self):
        commands = [self.command()]
        while self.at("|"):
            self.take()
            commands.append(self.command())
        return Node("pipeline", children=commands)

    def command(self):
        if self.at("if"):
            return self.conditional()
        if self.at("case"):
            return self.case()
        if self.at("{", "("):
            closing = "}" if self.take().value == "{" else ")"
            body = self.sequence(closing)
            self.take(closing)
            return Node("group", children=body)
        words = []
        while self.index < len(self.tokens) and not self.at(";", "&&", "||", "|", ";;", "(", ")", "&"):
            words.append(self.take())
        if not words:
            raise ValueError("empty or unsupported shell command")
        return Node("simple", words=words)

    def conditional(self):
        self.take("if")
        branches = []
        while True:
            condition = self.sequence("then")
            self.take("then")
            body = self.sequence("elif", "else", "fi")
            branches.append(Node("branch", children=[condition, body]))
            if not self.at("elif"):
                break
            self.take()
        if self.at("else"):
            self.take()
            branches.append(Node("branch", children=[[], self.sequence("fi")]))
        self.take("fi")
        return Node("if", children=branches)

    def case(self):
        self.take("case")
        self.take()
        self.take("in")
        branches = []
        while not self.at("esac"):
            while not self.at(")"):
                self.take()
            self.take(")")
            branches.append(self.sequence(";;", "esac"))
            if self.at(";;"):
                self.take()
        self.take("esac")
        return Node("case", children=branches)


def invocation(words):
    values = [word.value for word in words]
    while values and re.match(r"^[A-Za-z_]\w*=", values[0]):
        values.pop(0)
    return values


def is_curl(words):
    args = invocation(words)
    if not args:
        return any(re.search(r"\bcurl\b", word.value) for word in words)
    command = posixpath.basename(args[0])
    if command == "curl":
        return True
    if command in {"echo", "printf", "apt-get", "apt", "apk", "dnf", "yum", "microdnf", "npm", "pnpm", "pip", "pip3"}:
        return False
    return any(re.search(r"\bcurl\b", word) for word in args[1:])


def literal_path(value):
    if not re.fullmatch(r"[A-Za-z0-9_./+-]+", value) or value == "-":
        raise ValueError(f"unsupported download filename {value!r}; use a literal file path")
    path = posixpath.normpath(value)
    if path in {".", ".."} or path.startswith(("/dev/", "/proc/", "/sys/")):
        raise ValueError(f"unsupported download filename {value!r}; use a regular artifact file")
    return path


def curl_output(words):
    if words[0].value != "curl":
        raise ValueError("unsupported curl wrapper; invoke curl directly")
    args = [word.value for word in words[1:]]
    output, remote, urls = None, False, []
    while args:
        arg = args.pop(0)
        if arg in {"-o", "--output"}:
            if output is not None or not args:
                raise ValueError("unsupported curl outputs; use one URL and one file per command")
            output = args.pop(0)
        elif arg.startswith("--output=") or (arg.startswith("-o") and len(arg) > 2):
            if output is not None:
                raise ValueError("multiple curl outputs are unsupported")
            output = arg.split("=", 1)[1] if arg.startswith("--") else arg[2:]
        elif re.fullmatch(r"-[fsSLO]*o.*", arg):
            if output is not None:
                raise ValueError("multiple curl outputs are unsupported")
            flags, attached = arg[1:].split("o", 1)
            remote |= "O" in flags
            if not attached and not args:
                raise ValueError("curl -o needs an output filename")
            output = attached or args.pop(0)
        elif arg == "--remote-name" or re.fullmatch(r"-[fsSLO]+", arg):
            remote |= arg == "--remote-name" or "O" in arg
        elif arg in {"--fail", "--silent", "--show-error", "--location"}:
            pass
        elif arg.startswith("-"):
            raise ValueError(f"unsupported curl option {arg!r}")
        else:
            urls.append(arg)
    if len(urls) != 1 or (output is not None and remote):
        raise ValueError("unsupported curl form; use one URL and one explicit output")
    if remote:
        output = urls[0].rsplit("/", 1)[-1]
    if output is None:
        raise ValueError("curl stdout/direct consumption is unverified; download to a checked file")
    return literal_path(output)


def checksum_file(commands):
    if len(commands) != 2 or any(command.kind != "simple" for command in commands):
        return None
    producer, consumer = ([word.value for word in command.words] for command in commands)
    if not consumer or consumer[0] != "sha256sum":
        return None
    if consumer not in (["sha256sum", "-c", "-"], ["sha256sum", "--check", "-"]):
        raise ValueError("unsupported checksum options; use sha256sum -c -")
    if len(producer) == 2 and producer[0] == "echo":
        data = producer[1]
    elif len(producer) == 3 and producer[:2] == ["printf", "%s\\n"]:
        data = producer[2]
    else:
        raise ValueError("unsupported checksum input; pipe echo 'HASH  FILE' or printf '%s\\n' into sha256sum -c -")
    match = re.fullmatch(r"(?:[a-fA-F0-9]{64}|\$[A-Za-z_]\w*|\$\{[A-Za-z_]\w*\}) +\*?([^\s]+)", data)
    if not match:
        raise ValueError("checksum must name one hash and the downloaded file")
    return literal_path(match[1])


class Checker:
    def __init__(self, tree):
        self.pending = set()
        self.errexit = False
        self.first = tree[0].children[0].children[0] if tree else None
        if self.first and self.first.kind == "simple":
            args = [word.value for word in self.first.words]
            self.errexit = args in (["set", "-e"], ["set", "-eu"], ["set", "-ue"], ["set", "-eux"], ["set", "-o", "errexit"])

    def boundary(self):
        if self.pending:
            raise ValueError(f"curl download(s) {', '.join(sorted(self.pending))} need a same-file checksum before another command or branch")

    def sequence(self, tree, root=False, condition=False):
        for index, chain in enumerate(tree):
            protected = False
            if chain.ops:
                # An earlier && failure can skip the entire tail with errexit disabled.
                protected = root and index == len(tree) - 1 and all(op == "&&" for op in chain.ops)
            for pipeline in chain.children:
                if len(pipeline.children) > 1 and any(
                    command.kind == "simple" and any(re.search(r"\bcurl\b", word.value) for word in command.words)
                    for command in pipeline.children
                ):
                    raise ValueError("curl in a pipeline is unsupported; checksum the file before consumption")
                contains_curl = any(command.kind == "simple" and is_curl(command.words) for command in pipeline.children)
                checked = checksum_file(pipeline.children)
                if contains_curl or checked:
                    if condition or (chain.ops and not protected) or (not chain.ops and not self.errexit):
                        raise ValueError("download/checksum must fail closed: initial set -e with unconditional commands, or a terminal && chain")
                if contains_curl:
                    if len(pipeline.children) != 1:
                        raise ValueError("curl in a pipeline is unsupported; checksum the file before consumption")
                    path = curl_output(pipeline.children[0].words)
                    if path in self.pending:
                        raise ValueError(f"{path} is overwritten before its checksum")
                    self.pending.add(path)
                elif checked:
                    if checked not in self.pending:
                        raise ValueError(f"checksum for {checked} does not match a pending curl download")
                    self.pending.remove(checked)
                else:
                    self.boundary()
                    for command in pipeline.children:
                        self.command(command, condition or bool(chain.ops) or len(pipeline.children) != 1)
        self.boundary()

    def command(self, node, condition):
        if node.kind == "simple":
            args = invocation(node.words)
            if not args:
                return
            if args[0] == "set" and node is self.first and self.errexit:
                return
            if args[0] in {"set", "trap", "eval", "alias", "source", ".", "exec", "for", "while", "until", "select", "function", "!", "time"}:
                raise ValueError(f"unsupported {args[0]} in a curl RUN; keep download/check commands explicit")
        elif node.kind == "if":
            for branch in node.children:
                self.sequence(branch.children[0], condition=True)
                self.sequence(branch.children[1], condition=condition)
        elif node.kind == "case":
            for branch in node.children:
                self.sequence(branch, condition=condition)
        else:
            self.sequence(node.children, condition=True)


def has_download(node):
    if isinstance(node, list):
        return any(has_download(child) for child in node)
    if node.kind == "simple":
        return is_curl(node.words)
    if node.kind == "pipeline" and len(node.children) > 1:
        if any(command.kind == "simple" and any(re.search(r"\bcurl\b", word.value) for word in command.words)
               for command in node.children):
            return True
    return any(has_download(child) for child in node.children)


def check_run(body):
    if body.startswith("--"):
        raise ValueError("unsupported RUN option with curl; keep the checked download in an explicit shell RUN")
    if body.startswith("["):
        args = json.loads(body)
        if any(re.search(r"\bcurl\b", arg) for arg in args):
            raise ValueError("exec-form curl RUN is unsupported; use a shell RUN with an explicit checksum")
        return
    tokens = tokenize(body)
    for token in tokens:
        if "$(" in token.raw and re.search(r"\bcurl\b", token.value):
            raise ValueError("curl in command substitution is unverified; download to a checked file")
    parser = Parser(tokens)
    tree = parser.sequence()
    if has_download(tree):
        Checker(tree).sequence(tree, root=True)


def check_file(path):
    text = Path(path).read_text()
    custom_shell = False
    for number, name, body in instructions(text):
        if name == "SHELL":
            custom_shell = True
        if name != "RUN":
            continue
        try:
            if "<<" in body:
                raise ValueError("heredoc RUN is unsupported; use explicit continued RUN commands")
            if not re.search(r"\bcurl\b", body.replace("\\", "").replace("'", "").replace('"', "")):
                continue
            if custom_shell or re.search(r"^#\s*escape\s*=\s*`", text, re.M | re.I):
                raise ValueError("custom SHELL/escape with curl is unsupported")
            check_run(body)
        except (ValueError, IndexError) as error:
            raise ValueError(f"{path}:{number}: {error}") from error


if __name__ == "__main__":
    failed = False
    for filename in sys.argv[1:]:
        try:
            check_file(filename)
        except ValueError as error:
            print(f"FAIL: {error}", file=sys.stderr)
            failed = True
    sys.exit(int(failed))
