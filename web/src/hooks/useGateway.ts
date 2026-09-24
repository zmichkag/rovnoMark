import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';

export const useGatewayData = () => {
    const liveQuery = useQuery({
        queryKey: ['dashboard-live'],
        queryFn: api.getDashboardLive,
        refetchInterval: 2000,
        staleTime: 1500,
    });

    const topologyQuery = useQuery({
        queryKey: ['printers-topology'],
        queryFn: api.getPrintersTopology,
        refetchInterval: 5000,
        staleTime: 4000,
    });

    const logsQuery = useQuery({
        queryKey: ['logs-history'],
        queryFn: () => api.getLogsHistory(50),
        refetchInterval: 2000,
        staleTime: 1500,
    });

    return {
        live: liveQuery.data,
        topology: topologyQuery.data,
        logs: logsQuery.data?.items || [],
        isLoading: liveQuery.isLoading || topologyQuery.isLoading,
        isError: liveQuery.isError,
        error: liveQuery.error,
    };
};

export const useStopTask = () => {
    const queryClient = useQueryClient();
    return useMutation({
        mutationFn: (taskId: number) => api.stopTask(taskId),
        onSuccess: () => {
            queryClient.invalidateQueries({ queryKey: ['dashboard-live'] });
            queryClient.invalidateQueries({ queryKey: ['logs-history'] });
        },
    });
};