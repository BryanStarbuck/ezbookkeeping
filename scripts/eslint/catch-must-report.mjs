// errfile/catch-must-report — pm/error_err.mdx §13.2.
//
// Every place a failure is caught must either report it to the error file, hand it to a reporting
// net, rethrow it, or mark it expected. A catch that only logs to the console (or does nothing) is
// the silent failure the error file exists to end. Two companion checks keep the records useful:
// the `where` given to errorFileFor() must be this file's repo-relative path (R14), and the `doing`
// given to a report must be a stable string so burst folding (§11.1) has a key.
//
// A plain ESLint flat-config rule: no dependency beyond ESLint itself. It runs on .vue files
// because vue-eslint-parser exposes the <script> block as a standard ESTree program.

import fs from 'node:fs';
import path from 'node:path';

/** Methods on an ErrorFile object (§6.2). */
const REPORT_METHODS = new Set(['caught', 'warn', 'expected', 'rethrow', 'fatal']);

/** Free functions that report on the caller's behalf (§6.2). */
const REPORT_HELPERS = new Set(['tryOr', 'tryOrAsync', 'reportRejection', 'guard']);

/** Helpers whose `doing` is the second argument (`fn(errors, doing, …)`). */
const DOING_SECOND_ARG_HELPERS = REPORT_HELPERS;

/** The N4 net: `logger.error` / `logger.warn` in src/lib/logger.ts report on the caller's behalf. */
const LOGGER_NET_METHODS = new Set(['error', 'warn']);

/** Identifiers (or trailing member names) allowed inside a `doing` template literal (§13.2). */
const DOING_IDENTIFIERS = new Set(['name', 'method', 'path', 'route', 'tool', 'verb']);

/** `onerror` / `onabort` assigned a function: FileReader, Worker, Image, XMLHttpRequest (§13.2). */
const ERROR_PROPERTIES = new Set(['onerror', 'onabort']);

/** Event names whose listeners are error sites. */
const ERROR_EVENTS = new Set(['error', 'unhandledrejection']);

/** Files that mark the repo root, so `where` can be computed for an absolute filename. */
const ROOT_MARKERS = ['ezbookkeeping.go', 'go.mod'];

/** Keys of an AST node that are never children. */
const NON_CHILD_KEYS = new Set(['parent', 'loc', 'range', 'start', 'end', 'type', 'tokens', 'comments']);

function isErrorFileIdentifier(node) {
    return node && node.type === 'Identifier' && (node.name === 'errors' || node.name.endsWith('Errors'));
}

function isIdentifierNamed(node, name) {
    return node && node.type === 'Identifier' && node.name === name;
}

/** The plain property name of a non-computed member access, else null. */
function memberPropertyName(node) {
    if (node && node.type === 'MemberExpression' && !node.computed && node.property.type === 'Identifier') {
        return node.property.name;
    }

    return null;
}

function isFunctionNode(node) {
    return node && (node.type === 'ArrowFunctionExpression' || node.type === 'FunctionExpression');
}

function isStringLiteral(node) {
    return node && node.type === 'Literal' && typeof node.value === 'string';
}

/** True when `node` is a call that counts as reporting or handing off (§13.2). */
function isReportingCall(node) {
    if (node.type !== 'CallExpression') {
        return false;
    }

    const { callee } = node;

    if (callee.type === 'MemberExpression') {
        const prop = memberPropertyName(callee);

        if (prop === null) {
            return false;
        }

        if (REPORT_METHODS.has(prop) && isErrorFileIdentifier(callee.object)) {
            return true;
        }

        if (LOGGER_NET_METHODS.has(prop) && isIdentifierNamed(callee.object, 'logger')) {
            return true;
        }

        return REPORT_HELPERS.has(prop);
    }

    if (callee.type === 'Identifier') {
        return REPORT_HELPERS.has(callee.name);
    }

    return false;
}

/** `Promise.reject(...)` */
function isPromiseReject(node) {
    return node && node.type === 'CallExpression' && node.callee.type === 'MemberExpression' &&
        isIdentifierNamed(node.callee.object, 'Promise') && memberPropertyName(node.callee) === 'reject';
}

/** Walk a subtree (any depth) and return true as soon as `pred` matches a node. */
function subtreeSome(root, pred) {
    const stack = [root];

    while (stack.length > 0) {
        const node = stack.pop();

        if (!node || typeof node !== 'object') {
            continue;
        }

        if (Array.isArray(node)) {
            for (let i = node.length - 1; i >= 0; i--) {
                stack.push(node[i]);
            }

            continue;
        }

        if (typeof node.type !== 'string') {
            continue;
        }

        if (pred(node)) {
            return true;
        }

        for (const key of Object.keys(node)) {
            if (NON_CHILD_KEYS.has(key)) {
                continue;
            }

            const child = node[key];

            if (child && typeof child === 'object') {
                stack.push(child);
            }
        }
    }

    return false;
}

function nodePasses(node) {
    if (node.type === 'ThrowStatement') {
        return true;
    }

    if (node.type === 'ReturnStatement' && isPromiseReject(node.argument)) {
        return true;
    }

    return isReportingCall(node);
}

/**
 * Does a handler body report? `body` is a BlockStatement, or the expression of an
 * expression-bodied arrow (which is an implicit return). A `reject(…)` alone does not count: it
 * moves the fault without recording it; with a `logger.*` / `errors.*` call beside it, that call
 * already makes the handler pass (upstream's store idiom).
 */
function handlerReports(body) {
    if (!body) {
        return false;
    }

    if (body.type !== 'BlockStatement' && isPromiseReject(body)) {
        return true;
    }

    return subtreeSome(body, nodePasses);
}

function isEmptyBlock(body) {
    return body && body.type === 'BlockStatement' && body.body.length === 0;
}

/** `def.name` — the one member chain the spec allows by its root. */
function isDefName(node) {
    return node.type === 'MemberExpression' && !node.computed && isIdentifierNamed(node.object, 'def') && memberPropertyName(node) === 'name';
}

/** `req.method`, `this.name`, `route.path`, `r.route` — a non-computed chain ending in an allowed name. */
function isAllowedMemberChain(node) {
    if (node.type !== 'MemberExpression') {
        return false;
    }

    const last = memberPropertyName(node);

    if (last === null || !DOING_IDENTIFIERS.has(last)) {
        return false;
    }

    let cur = node.object;

    while (cur.type === 'MemberExpression' && !cur.computed) {
        cur = cur.object;
    }

    return cur.type === 'Identifier' || cur.type === 'ThisExpression';
}

function isAllowedDoingExpression(expr) {
    if (expr.type === 'Identifier') {
        return DOING_IDENTIFIERS.has(expr.name);
    }

    return isDefName(expr) || isAllowedMemberChain(expr);
}

function isStableDoing(node) {
    if (!node) {
        return false;
    }

    if (isStringLiteral(node)) {
        return true;
    }

    if (node.type === 'TemplateLiteral') {
        return node.expressions.every(isAllowedDoingExpression);
    }

    return false;
}

/** The `doing` argument of a reporting call, or undefined when the call has none. */
function doingArgument(node) {
    const { callee } = node;

    if (callee.type === 'MemberExpression') {
        if (REPORT_METHODS.has(memberPropertyName(callee)) && isErrorFileIdentifier(callee.object)) {
            return node.arguments[0];
        }

        return undefined;
    }

    if (callee.type === 'Identifier' && DOING_SECOND_ARG_HELPERS.has(callee.name) && isErrorFileIdentifier(node.arguments[0])) {
        return node.arguments[1];
    }

    return undefined;
}

function findRepoRoot(startDir) {
    let dir = startDir;

    for (;;) {
        if (ROOT_MARKERS.every(m => fs.existsSync(path.join(dir, m)))) {
            return dir;
        }

        const parent = path.dirname(dir);

        if (parent === dir) {
            return null;
        }

        dir = parent;
    }
}

/**
 * The `where` this file must pass to errorFileFor(): its repo-relative path (R14). Returns null
 * when the file has no usable path (stdin, a virtual file).
 */
export function expectedWhere(filename) {
    if (!filename || filename.startsWith('<')) {
        return null;
    }

    let rel = filename.replace(/\\/g, '/');

    if (path.isAbsolute(filename)) {
        const root = findRepoRoot(path.dirname(filename));

        if (!root) {
            return null;
        }

        rel = path.relative(root, filename).replace(/\\/g, '/');
    }

    rel = rel.replace(/^(\.\/)+/, '');

    if (rel.startsWith('..')) {
        return null;
    }

    return rel;
}

//------------------------------------------------------------------------------
// Rule Definition
//------------------------------------------------------------------------------

/** @type {import('eslint').Rule.RuleModule} */
const rule = {
    meta: {
        type: 'problem',
        docs: {
            description: 'Every catch, .catch(), rejection handler, onerror and error listener must report to the error file, rethrow, or mark the failure expected (pm/error_err.mdx §2 R1, §13.2)'
        },
        fixable: 'code',
        schema: [],
        messages: {
            unreported: 'This catch neither reports (errors.caught / warn / expected / rethrow / fatal, tryOr, guard, logger.error), rethrows, nor marks the failure expected — see pm/error_err.mdx §7',
            emptyCatch: 'Empty catch swallows the failure — call errors.expected(…) if it is expected, otherwise errors.caught(…) — see pm/error_err.mdx §7 T3',
            wrongWhere: 'errorFileFor() must be given this file\'s repo-relative path \'{{expected}}\' — see pm/error_err.mdx R14',
            dynamicDoing: 'The `doing` argument must be a string literal (or a template whose only expressions are name, method, path, route, def.name, tool or verb) so the fold key stays stable — see pm/error_err.mdx §6.2'
        }
    },

    create(context) {
        const expected = expectedWhere(context.filename);

        function reportHandler(site, body) {
            if (isEmptyBlock(body)) {
                context.report({ node: site, messageId: 'emptyCatch' });
            } else if (!handlerReports(body)) {
                context.report({ node: site, messageId: 'unreported' });
            }
        }

        /** Check an inline function handler; other shapes (a named handler) are opaque and pass. */
        function checkHandlerArgument(site, handler) {
            if (!handler) {
                return;
            }

            if (isFunctionNode(handler)) {
                reportHandler(site, handler.body);
                return;
            }

            // `.catch(console.error)` is the console-only report T5 replaces.
            if (handler.type === 'MemberExpression' && isIdentifierNamed(handler.object, 'console')) {
                context.report({ node: site, messageId: 'unreported' });
            }
        }

        function checkWhere(node) {
            if (expected === null) {
                return;
            }

            const arg = node.arguments[0];

            if (isStringLiteral(arg) && arg.value === expected) {
                return;
            }

            context.report({
                node: arg || node,
                messageId: 'wrongWhere',
                data: { expected },
                fix(fixer) {
                    const text = `'${expected}'`;
                    return arg ? fixer.replaceText(arg, text) : fixer.replaceText(node, `errorFileFor(${text})`);
                }
            });
        }

        function checkDoing(node) {
            const doing = doingArgument(node);

            if (doing === undefined) {
                return;
            }

            if (!isStableDoing(doing)) {
                context.report({ node: doing, messageId: 'dynamicDoing' });
            }
        }

        return {
            CatchClause(node) {
                reportHandler(node, node.body);
            },

            CallExpression(node) {
                const { callee } = node;

                if (isIdentifierNamed(callee, 'errorFileFor')) {
                    checkWhere(node);
                    return;
                }

                checkDoing(node);

                if (callee.type !== 'MemberExpression') {
                    return;
                }

                const prop = memberPropertyName(callee);

                if (prop === 'catch') {
                    checkHandlerArgument(node, node.arguments[0]);
                } else if (prop === 'then') {
                    if (node.arguments.length >= 2) {
                        checkHandlerArgument(node, node.arguments[1]);
                    }
                } else if (prop === 'addEventListener' && isStringLiteral(node.arguments[0]) && ERROR_EVENTS.has(node.arguments[0].value)) {
                    checkHandlerArgument(node, node.arguments[1]);
                }
            },

            AssignmentExpression(node) {
                if (node.operator !== '=' || node.left.type !== 'MemberExpression') {
                    return;
                }

                const prop = memberPropertyName(node.left);

                if (prop === null || !ERROR_PROPERTIES.has(prop)) {
                    return;
                }

                if (isFunctionNode(node.right)) {
                    reportHandler(node, node.right.body);
                }
            }
        };
    }
};

export default rule;
