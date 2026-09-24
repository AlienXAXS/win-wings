import React, { useEffect, useState } from 'react';
import http from '@/api/http';
import { ServerContext } from '@/state/server';

interface StartupResponse {
    windows: boolean;
    profiled?: boolean;
    overridden?: boolean;
    invocation?: string | null;
}

/**
 * Sits above the Startup page on servers hosted by a win-wings node.
 *
 * That page shows the egg's startup line and Docker image, and on Windows
 * neither is necessarily true: a Windows profile can replace the command, and
 * there is no container to pick an image for. The core boxes cannot be removed
 * from here, so this says which parts to disregard and, where the command is
 * replaced, shows the one that actually runs.
 *
 * Renders nothing at all on any other node, and nothing while loading or on an
 * error -- the page is still correct for everything it does not mention.
 */
export default () => {
    const uuid = ServerContext.useStoreState((state) => state.server.data?.uuid);

    // Changes when a variable is saved on this page. Refetching on it keeps the
    // Windows line's substituted values in step with the Panel's.
    const invocation = ServerContext.useStoreState((state) => state.server.data?.invocation);

    const [data, setData] = useState<StartupResponse | null>(null);

    useEffect(() => {
        if (!uuid) return;

        let cancelled = false;

        http.get(`/api/client/extensions/axwinwings/servers/${uuid}/startup`)
            .then((response) => {
                if (!cancelled) setData(response.data);
            })
            .catch((error) => {
                // Nothing on the page: without an answer it is no worse than
                // before. The console is where anyone debugging a missing notice
                // will look, though.
                console.warn('[axwinwings] could not load the Windows startup notice', error);
            });

        return () => {
            cancelled = true;
        };
    }, [uuid, invocation]);

    if (!data || !data.windows) {
        return null;
    }

    if (!data.profiled) {
        return (
            <div className={'mb-6 rounded-lg p-4 shadow-lg'} style={{ backgroundColor: '#92400e' }}>
                <h3 className={'text-white font-semibold text-lg'}>This server runs on Windows</h3>
                <p className={'text-sm mt-1'} style={{ color: '#fef3c7' }}>
                    Its game has not been set up for Windows yet, so the server will not start. The startup command and
                    Docker image below are for Linux and do not apply. Please contact an administrator.
                </p>
            </div>
        );
    }

    return (
        <div className={'mb-6 rounded-lg p-4 shadow-lg'} style={{ backgroundColor: '#1e3a8a' }}>
            <h3 className={'text-white font-semibold text-lg'}>This server runs on Windows</h3>
            {data.overridden && data.invocation ? (
                <>
                    <p className={'text-sm mt-1'} style={{ color: '#dbeafe' }}>
                        The startup command below is the Linux one and is not used. This server starts with:
                    </p>
                    <p className={'font-mono bg-neutral-900 rounded py-2 px-4 mt-3 text-neutral-100 break-all'}>
                        {data.invocation}
                    </p>
                    <p className={'text-xs mt-3'} style={{ color: '#bfdbfe' }}>
                        There is no Docker container on Windows, so the Docker image setting has no effect.
                    </p>
                </>
            ) : (
                <p className={'text-sm mt-1'} style={{ color: '#dbeafe' }}>
                    It starts with the command shown below. There is no Docker container on Windows, so the Docker image
                    setting has no effect.
                </p>
            )}
        </div>
    );
};
