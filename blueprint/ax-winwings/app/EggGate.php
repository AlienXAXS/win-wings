<?php

namespace Pterodactyl\BlueprintFramework\Extensions\{identifier};

use Illuminate\Support\Arr;
use Pterodactyl\Exceptions\DisplayException;
use Pterodactyl\Models\Allocation;
use Pterodactyl\Models\Egg;

/**
 * Refuses to create a server whose egg has no Windows profile on a Windows node.
 *
 * The daemon already refuses it — but only after the Panel has written the
 * record, allocated a port and told the user their server is installing. What
 * they then see is a server stuck in "installing" forever, with the real reason
 * only in the node's log. Failing at selection time turns that into a sentence
 * the person reading it can act on.
 *
 * Called from ServerCreationService::handle() by a one-line patch that
 * data/install.sh applies; the extension has no other way to sit in front of
 * creation. It is deliberately the last thing that can be turned off: the gate
 * is a usability feature, and an operator who wants to create a server the
 * daemon will reject should be able to.
 */
class EggGate
{
    /**
     * @param array $data the raw creation payload, after ServerCreationService has
     *                    resolved node_id and nest_id but before anything is written
     *
     * @throws DisplayException
     */
    public static function check(array $data): void
    {
        if (!WinWings::ready() || !WinWings::flag('gate_enabled', true)) {
            return;
        }

        $nodeId = (int) Arr::get($data, 'node_id', 0);
        $eggId = (int) Arr::get($data, 'egg_id', 0);

        // A creation reaching here with no node_id is an allocation-only payload
        // the service is about to resolve itself, and one with no egg_id is going
        // to fail its own assertion in a moment. Neither is ours to report on.
        if ($nodeId <= 0) {
            $allocationId = (int) Arr::get($data, 'allocation_id', 0);

            if ($allocationId <= 0) {
                return;
            }

            $nodeId = (int) Allocation::query()->where('id', $allocationId)->value('node_id');
        }

        if ($nodeId <= 0 || $eggId <= 0) {
            return;
        }

        if (!WinWings::isWindowsNode($nodeId)) {
            return;
        }

        if (WinWings::profileForEgg($eggId)) {
            return;
        }

        $egg = Egg::query()->find($eggId);
        $message = trim((string) WinWings::setting('gate_message', 'This egg is not available on Windows nodes.'));

        if ($message === '') {
            $message = 'This egg is not available on Windows nodes.';
        }

        // The egg name is appended rather than interpolated into the operator's
        // message so that editing the message cannot accidentally drop the one
        // detail that makes the error actionable.
        if ($egg) {
            $message .= ' (' . $egg->name . ' has no Windows profile.)';
        }

        throw new DisplayException($message);
    }
}
