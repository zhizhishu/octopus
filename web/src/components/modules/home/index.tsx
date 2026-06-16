'use client';

import { Activity } from './activity';
import { Total } from './total';
import { StatsChart } from './chart';
import { Rank } from './rank';
import { ModelHealth } from './model-health';
import { CheckInCard } from './check-in';
import { PageWrapper } from '@/components/common/PageWrapper';

export function Home() {
    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-6 pb-24 md:pb-4 rounded-t-3xl">
            <Total />
            <CheckInCard />
            <Activity />
            <ModelHealth />
            <StatsChart />
            <Rank />
        </PageWrapper>
    );
}
