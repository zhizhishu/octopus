'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { useTranslations } from 'next-intl';
import { Check, Clock, Fingerprint, Gift, Globe, HelpCircle, Loader2, Mail, Monitor, Pencil, Radio, Shield, UserPlus, X, Zap, AlertTriangle } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import { Button } from '@/components/ui/button';
import {
    Dialog,
    DialogContent,
    DialogDescription,
    DialogFooter,
    DialogHeader,
    DialogTitle,
} from '@/components/ui/dialog';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { useSettingList, useSetSetting, SettingKey, SECRET_MASK } from '@/api/endpoints/setting';
import { toast } from '@/components/common/Toast';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';

type HeaderProfile = 'claude' | 'codex';

type HeaderDraft = {
    claudeUserAgent: string;
    claudePackageVersion: string;
    claudeRuntimeVersion: string;
    claudeOS: string;
    claudeArch: string;
    claudeTimeout: string;
    claudeStabilizeDeviceProfile: boolean;
    codexUserAgent: string;
    codexBetaFeatures: string;
};

export function SettingSystem() {
    const t = useTranslations('setting');
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();

    const [proxyUrl, setProxyUrl] = useState('');
    const [statsSaveInterval, setStatsSaveInterval] = useState('');
    const [corsAllowOrigins, setCorsAllowOrigins] = useState('');
    const [corsInputValue, setCorsInputValue] = useState('');
    const [anthropicAutoCacheControl, setAnthropicAutoCacheControl] = useState(true);
    const [openAIAutoPromptCacheKey, setOpenAIAutoPromptCacheKey] = useState(true);
    const [streamKeepaliveInterval, setStreamKeepaliveInterval] = useState('15');
    const [streamDataTimeoutInterval, setStreamDataTimeoutInterval] = useState('900');
    const [responsesSessionTTL, setResponsesSessionTTL] = useState('3600');
    const [claudeHeaderUserAgent, setClaudeHeaderUserAgent] = useState('claude-cli/2.1.168 (external, sdk-cli)');
    const [claudeHeaderPackageVersion, setClaudeHeaderPackageVersion] = useState('0.94.0');
    const [claudeHeaderRuntimeVersion, setClaudeHeaderRuntimeVersion] = useState('v24.3.0');
    const [claudeHeaderOS, setClaudeHeaderOS] = useState('Windows');
    const [claudeHeaderArch, setClaudeHeaderArch] = useState('x64');
    const [claudeHeaderTimeout, setClaudeHeaderTimeout] = useState('600');
    const [claudeHeaderStabilizeDeviceProfile, setClaudeHeaderStabilizeDeviceProfile] = useState(true);
    const [claudeCLIAutoCompact, setClaudeCLIAutoCompact] = useState(false);
    const [claudeCLIReasoningEffort, setClaudeCLIReasoningEffort] = useState('auto');
    const [claudeBetaStripFlags, setClaudeBetaStripFlags] = useState('');
    const [codexHeaderUserAgent, setCodexHeaderUserAgent] = useState('codex_exec/0.132.0 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.132.0)');
    const [codexHeaderBetaFeatures, setCodexHeaderBetaFeatures] = useState('terminal_resize_reflow');
    const [codexFastMode, setCodexFastMode] = useState(false);
    const [editingHeaderProfile, setEditingHeaderProfile] = useState<HeaderProfile | null>(null);
    const [headerDraft, setHeaderDraft] = useState<HeaderDraft>({
        claudeUserAgent: 'claude-cli/2.1.168 (external, sdk-cli)',
        claudePackageVersion: '0.94.0',
        claudeRuntimeVersion: 'v24.3.0',
        claudeOS: 'Windows',
        claudeArch: 'x64',
        claudeTimeout: '600',
        claudeStabilizeDeviceProfile: true,
        codexUserAgent: 'codex_exec/0.132.0 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.132.0)',
        codexBetaFeatures: 'terminal_resize_reflow',
    });
    const [userRegistrationEnabled, setUserRegistrationEnabled] = useState(false);
    const [upstreamErrorStatusPassthrough, setUpstreamErrorStatusPassthrough] = useState(true);
    const [upstreamErrorBodyMode, setUpstreamErrorBodyMode] = useState('redacted_upstream');
    const [upstreamErrorCustomMessage, setUpstreamErrorCustomMessage] = useState('');
    const [upstreamErrorPublicCode, setUpstreamErrorPublicCode] = useState('service_busy');
    const [checkInEnabled, setCheckInEnabled] = useState(false);
    const [checkInRewardMode, setCheckInRewardMode] = useState('fixed');
    const [checkInRewardAmount, setCheckInRewardAmount] = useState('100');
    const [checkInRewardMin, setCheckInRewardMin] = useState('100');
    const [checkInRewardMax, setCheckInRewardMax] = useState('200');
    const [emailVerificationEnabled, setEmailVerificationEnabled] = useState(false);
    const [emailProvider, setEmailProvider] = useState('smtp');
    const [emailSMTPHost, setEmailSMTPHost] = useState('');
    const [emailSMTPPort, setEmailSMTPPort] = useState('587');
    const [emailSMTPUser, setEmailSMTPUser] = useState('');
    const [emailSMTPPassword, setEmailSMTPPassword] = useState('');
    const [emailSMTPFrom, setEmailSMTPFrom] = useState('');
    const [emailSMTPFromName, setEmailSMTPFromName] = useState('Octopus');
    const [emailSMTPSSL, setEmailSMTPSSL] = useState(false);
    const [emailHTTPBaseURL, setEmailHTTPBaseURL] = useState('');
    const [emailHTTPFrom, setEmailHTTPFrom] = useState('');
    const [emailHTTPAdminAuth, setEmailHTTPAdminAuth] = useState('');
    const [emailHTTPSiteAuth, setEmailHTTPSiteAuth] = useState('');

    const initialProxyUrl = useRef('');
    const initialStatsSaveInterval = useRef('');
    const initialCorsAllowOrigins = useRef('');
    const initialAnthropicAutoCacheControl = useRef(true);
    const initialOpenAIAutoPromptCacheKey = useRef(true);
    const initialStreamKeepaliveInterval = useRef('15');
    const initialStreamDataTimeoutInterval = useRef('900');
    const initialResponsesSessionTTL = useRef('3600');
    const initialClaudeHeaderUserAgent = useRef('claude-cli/2.1.168 (external, sdk-cli)');
    const initialClaudeHeaderPackageVersion = useRef('0.94.0');
    const initialClaudeHeaderRuntimeVersion = useRef('v24.3.0');
    const initialClaudeHeaderOS = useRef('Windows');
    const initialClaudeHeaderArch = useRef('x64');
    const initialClaudeHeaderTimeout = useRef('600');
    const initialClaudeHeaderStabilizeDeviceProfile = useRef(true);
    const initialClaudeCLIAutoCompact = useRef(false);
    const initialClaudeCLIReasoningEffort = useRef('auto');
    const initialClaudeBetaStripFlags = useRef('');
    const initialCodexHeaderUserAgent = useRef('codex_exec/0.132.0 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.132.0)');
    const initialCodexHeaderBetaFeatures = useRef('terminal_resize_reflow');
    const initialCodexFastMode = useRef(false);
    const initialUserRegistrationEnabled = useRef(false);
    const initialUpstreamErrorStatusPassthrough = useRef(true);
    const initialUpstreamErrorBodyMode = useRef('redacted_upstream');
    const initialUpstreamErrorCustomMessage = useRef('');
    const initialUpstreamErrorPublicCode = useRef('service_busy');
    const initialCheckInEnabled = useRef(false);
    const initialCheckInRewardMode = useRef('fixed');
    const initialCheckInRewardAmount = useRef('100');
    const initialCheckInRewardMin = useRef('100');
    const initialCheckInRewardMax = useRef('200');
    const initialEmailVerificationEnabled = useRef(false);
    const initialEmailProvider = useRef('smtp');
    const initialEmailSMTPHost = useRef('');
    const initialEmailSMTPPort = useRef('587');
    const initialEmailSMTPUser = useRef('');
    const initialEmailSMTPPassword = useRef('');
    const initialEmailSMTPFrom = useRef('');
    const initialEmailSMTPFromName = useRef('Octopus');
    const initialEmailSMTPSSL = useRef(false);
    const initialEmailHTTPBaseURL = useRef('');
    const initialEmailHTTPFrom = useRef('');
    const initialEmailHTTPAdminAuth = useRef('');
    const initialEmailHTTPSiteAuth = useRef('');

    useEffect(() => {
        if (settings) {
            const proxy = settings.find(s => s.key === SettingKey.ProxyURL);
            const interval = settings.find(s => s.key === SettingKey.StatsSaveInterval);
            const cors = settings.find(s => s.key === SettingKey.CORSAllowOrigins);
            const autoCache = settings.find(s => s.key === SettingKey.AnthropicAutoCacheControl);
            const openAIAutoCacheKey = settings.find(s => s.key === SettingKey.OpenAIAutoPromptCacheKey);
            const keepalive = settings.find(s => s.key === SettingKey.RelayStreamKeepaliveIntervalSeconds);
            const dataTimeout = settings.find(s => s.key === SettingKey.RelayStreamDataIntervalTimeoutSeconds);
            const responsesTTL = settings.find(s => s.key === SettingKey.ResponsesSessionTTLSeconds);
            const claudeUA = settings.find(s => s.key === SettingKey.ClaudeHeaderUserAgent);
            const claudePackage = settings.find(s => s.key === SettingKey.ClaudeHeaderPackageVersion);
            const claudeRuntime = settings.find(s => s.key === SettingKey.ClaudeHeaderRuntimeVersion);
            const claudeOS = settings.find(s => s.key === SettingKey.ClaudeHeaderOS);
            const claudeArch = settings.find(s => s.key === SettingKey.ClaudeHeaderArch);
            const claudeTimeout = settings.find(s => s.key === SettingKey.ClaudeHeaderTimeout);
            const claudeStabilize = settings.find(s => s.key === SettingKey.ClaudeHeaderStabilizeDeviceProfile);
            const claudeAutoCompact = settings.find(s => s.key === SettingKey.ClaudeCLIAutoCompact);
            const claudeReasoningEffort = settings.find(s => s.key === SettingKey.ClaudeCLIReasoningEffort);
            const claudeBetaStrip = settings.find(s => s.key === SettingKey.ClaudeBetaStripFlags);
            const codexUA = settings.find(s => s.key === SettingKey.CodexHeaderUserAgent);
            const codexBetaFeatures = settings.find(s => s.key === SettingKey.CodexHeaderBetaFeatures);
            const codexFast = settings.find(s => s.key === SettingKey.CodexFastMode);
            const registration = settings.find(s => s.key === SettingKey.UserRegistrationEnabled);
            const errorStatusPassthrough = settings.find(s => s.key === SettingKey.UpstreamErrorStatusPassthrough);
            const errorBodyMode = settings.find(s => s.key === SettingKey.UpstreamErrorBodyMode);
            const errorCustomMessage = settings.find(s => s.key === SettingKey.UpstreamErrorCustomMessage);
            const errorPublicCode = settings.find(s => s.key === SettingKey.UpstreamErrorPublicCode);
            const checkInEnabledSetting = settings.find(s => s.key === SettingKey.CheckInEnabled);
            const checkInMode = settings.find(s => s.key === SettingKey.CheckInRewardMode);
            const checkInAmount = settings.find(s => s.key === SettingKey.CheckInRewardAmount);
            const checkInMin = settings.find(s => s.key === SettingKey.CheckInRewardMin);
            const checkInMax = settings.find(s => s.key === SettingKey.CheckInRewardMax);
            const emailVerification = settings.find(s => s.key === SettingKey.EmailVerificationEnabled);
            const emailProviderSetting = settings.find(s => s.key === SettingKey.EmailProvider);
            const emailHost = settings.find(s => s.key === SettingKey.EmailSMTPHost);
            const emailPort = settings.find(s => s.key === SettingKey.EmailSMTPPort);
            const emailUser = settings.find(s => s.key === SettingKey.EmailSMTPUser);
            const emailPassword = settings.find(s => s.key === SettingKey.EmailSMTPPassword);
            const emailFrom = settings.find(s => s.key === SettingKey.EmailSMTPFrom);
            const emailFromName = settings.find(s => s.key === SettingKey.EmailSMTPFromName);
            const emailSSL = settings.find(s => s.key === SettingKey.EmailSMTPSSL);
            const emailHTTPBase = settings.find(s => s.key === SettingKey.EmailHTTPBaseURL);
            const emailHTTPFromSetting = settings.find(s => s.key === SettingKey.EmailHTTPFrom);
            const emailHTTPAdmin = settings.find(s => s.key === SettingKey.EmailHTTPAdminAuth);
            const emailHTTPSite = settings.find(s => s.key === SettingKey.EmailHTTPSiteAuth);
            if (proxy) {
                queueMicrotask(() => setProxyUrl(proxy.value));
                initialProxyUrl.current = proxy.value;
            }
            if (interval) {
                queueMicrotask(() => setStatsSaveInterval(interval.value));
                initialStatsSaveInterval.current = interval.value;
            }
            if (cors) {
                queueMicrotask(() => setCorsAllowOrigins(cors.value));
                initialCorsAllowOrigins.current = cors.value;
            }
            if (autoCache) {
                const enabled = autoCache.value === 'true';
                queueMicrotask(() => setAnthropicAutoCacheControl(enabled));
                initialAnthropicAutoCacheControl.current = enabled;
            }
            if (openAIAutoCacheKey) {
                const enabled = openAIAutoCacheKey.value === 'true';
                queueMicrotask(() => setOpenAIAutoPromptCacheKey(enabled));
                initialOpenAIAutoPromptCacheKey.current = enabled;
            }
            if (keepalive) {
                queueMicrotask(() => setStreamKeepaliveInterval(keepalive.value || '15'));
                initialStreamKeepaliveInterval.current = keepalive.value || '15';
            }
            if (dataTimeout) {
                queueMicrotask(() => setStreamDataTimeoutInterval(dataTimeout.value || '900'));
                initialStreamDataTimeoutInterval.current = dataTimeout.value || '900';
            }
            if (responsesTTL) {
                queueMicrotask(() => setResponsesSessionTTL(responsesTTL.value || '3600'));
                initialResponsesSessionTTL.current = responsesTTL.value || '3600';
            }
            if (claudeUA) {
                queueMicrotask(() => setClaudeHeaderUserAgent(claudeUA.value));
                initialClaudeHeaderUserAgent.current = claudeUA.value;
            }
            if (claudePackage) {
                queueMicrotask(() => setClaudeHeaderPackageVersion(claudePackage.value));
                initialClaudeHeaderPackageVersion.current = claudePackage.value;
            }
            if (claudeRuntime) {
                queueMicrotask(() => setClaudeHeaderRuntimeVersion(claudeRuntime.value));
                initialClaudeHeaderRuntimeVersion.current = claudeRuntime.value;
            }
            if (claudeOS) {
                queueMicrotask(() => setClaudeHeaderOS(claudeOS.value));
                initialClaudeHeaderOS.current = claudeOS.value;
            }
            if (claudeArch) {
                queueMicrotask(() => setClaudeHeaderArch(claudeArch.value));
                initialClaudeHeaderArch.current = claudeArch.value;
            }
            if (claudeTimeout) {
                queueMicrotask(() => setClaudeHeaderTimeout(claudeTimeout.value));
                initialClaudeHeaderTimeout.current = claudeTimeout.value;
            }
            if (claudeStabilize) {
                const enabled = claudeStabilize.value === 'true';
                queueMicrotask(() => setClaudeHeaderStabilizeDeviceProfile(enabled));
                initialClaudeHeaderStabilizeDeviceProfile.current = enabled;
            }
            if (claudeAutoCompact) {
                const enabled = claudeAutoCompact.value !== 'false';
                queueMicrotask(() => setClaudeCLIAutoCompact(enabled));
                initialClaudeCLIAutoCompact.current = enabled;
            }
            if (claudeReasoningEffort) {
                const rawValue = (claudeReasoningEffort.value || 'auto').toLowerCase();
                const value = ['auto', 'off', 'low', 'medium', 'high'].includes(rawValue) ? rawValue : 'auto';
                queueMicrotask(() => setClaudeCLIReasoningEffort(value));
                initialClaudeCLIReasoningEffort.current = value;
            }
            if (claudeBetaStrip) {
                queueMicrotask(() => setClaudeBetaStripFlags(claudeBetaStrip.value));
                initialClaudeBetaStripFlags.current = claudeBetaStrip.value;
            }
            if (codexUA) {
                queueMicrotask(() => setCodexHeaderUserAgent(codexUA.value));
                initialCodexHeaderUserAgent.current = codexUA.value;
            }
            if (codexBetaFeatures) {
                queueMicrotask(() => setCodexHeaderBetaFeatures(codexBetaFeatures.value));
                initialCodexHeaderBetaFeatures.current = codexBetaFeatures.value;
            }
            if (codexFast) {
                const enabled = codexFast.value !== 'false';
                queueMicrotask(() => setCodexFastMode(enabled));
                initialCodexFastMode.current = enabled;
            }
            if (registration) {
                const enabled = registration.value === 'true';
                queueMicrotask(() => setUserRegistrationEnabled(enabled));
                initialUserRegistrationEnabled.current = enabled;
            }
            if (errorStatusPassthrough) {
                const enabled = errorStatusPassthrough.value === 'true';
                queueMicrotask(() => setUpstreamErrorStatusPassthrough(enabled));
                initialUpstreamErrorStatusPassthrough.current = enabled;
            }
            if (errorBodyMode) {
                queueMicrotask(() => setUpstreamErrorBodyMode(errorBodyMode.value || 'redacted_upstream'));
                initialUpstreamErrorBodyMode.current = errorBodyMode.value || 'redacted_upstream';
            }
            if (errorCustomMessage) {
                queueMicrotask(() => setUpstreamErrorCustomMessage(errorCustomMessage.value));
                initialUpstreamErrorCustomMessage.current = errorCustomMessage.value;
            }
            if (errorPublicCode) {
                queueMicrotask(() => setUpstreamErrorPublicCode(errorPublicCode.value));
                initialUpstreamErrorPublicCode.current = errorPublicCode.value;
            }
            if (checkInEnabledSetting) {
                const enabled = checkInEnabledSetting.value === 'true';
                queueMicrotask(() => setCheckInEnabled(enabled));
                initialCheckInEnabled.current = enabled;
            }
            if (checkInMode) {
                queueMicrotask(() => setCheckInRewardMode(checkInMode.value || 'fixed'));
                initialCheckInRewardMode.current = checkInMode.value || 'fixed';
            }
            if (checkInAmount) {
                queueMicrotask(() => setCheckInRewardAmount(checkInAmount.value));
                initialCheckInRewardAmount.current = checkInAmount.value;
            }
            if (checkInMin) {
                queueMicrotask(() => setCheckInRewardMin(checkInMin.value));
                initialCheckInRewardMin.current = checkInMin.value;
            }
            if (checkInMax) {
                queueMicrotask(() => setCheckInRewardMax(checkInMax.value));
                initialCheckInRewardMax.current = checkInMax.value;
            }
            if (emailVerification) {
                const enabled = emailVerification.value === 'true';
                queueMicrotask(() => setEmailVerificationEnabled(enabled));
                initialEmailVerificationEnabled.current = enabled;
            }
            if (emailProviderSetting) {
                const provider = emailProviderSetting.value === 'http' ? 'http' : 'smtp';
                queueMicrotask(() => setEmailProvider(provider));
                initialEmailProvider.current = provider;
            }
            if (emailHost) {
                queueMicrotask(() => setEmailSMTPHost(emailHost.value));
                initialEmailSMTPHost.current = emailHost.value;
            }
            if (emailPort) {
                queueMicrotask(() => setEmailSMTPPort(emailPort.value || '587'));
                initialEmailSMTPPort.current = emailPort.value || '587';
            }
            if (emailUser) {
                queueMicrotask(() => setEmailSMTPUser(emailUser.value));
                initialEmailSMTPUser.current = emailUser.value;
            }
            if (emailPassword) {
                queueMicrotask(() => setEmailSMTPPassword(emailPassword.value));
                initialEmailSMTPPassword.current = emailPassword.value;
            }
            if (emailFrom) {
                queueMicrotask(() => setEmailSMTPFrom(emailFrom.value));
                initialEmailSMTPFrom.current = emailFrom.value;
            }
            if (emailFromName) {
                queueMicrotask(() => setEmailSMTPFromName(emailFromName.value || 'Octopus'));
                initialEmailSMTPFromName.current = emailFromName.value || 'Octopus';
            }
            if (emailSSL) {
                const enabled = emailSSL.value === 'true';
                queueMicrotask(() => setEmailSMTPSSL(enabled));
                initialEmailSMTPSSL.current = enabled;
            }
            if (emailHTTPBase) {
                queueMicrotask(() => setEmailHTTPBaseURL(emailHTTPBase.value));
                initialEmailHTTPBaseURL.current = emailHTTPBase.value;
            }
            if (emailHTTPFromSetting) {
                queueMicrotask(() => setEmailHTTPFrom(emailHTTPFromSetting.value));
                initialEmailHTTPFrom.current = emailHTTPFromSetting.value;
            }
            if (emailHTTPAdmin) {
                queueMicrotask(() => setEmailHTTPAdminAuth(emailHTTPAdmin.value));
                initialEmailHTTPAdminAuth.current = emailHTTPAdmin.value;
            }
            if (emailHTTPSite) {
                queueMicrotask(() => setEmailHTTPSiteAuth(emailHTTPSite.value));
                initialEmailHTTPSiteAuth.current = emailHTTPSite.value;
            }
        }
    }, [settings]);

    const handleSave = (key: string, value: string, initialValue: string) => {
        if (value === initialValue) return;

        setSetting.mutate({ key, value }, {
            onSuccess: () => {
                toast.success(t('saved'));
                if (key === SettingKey.ProxyURL) {
                    initialProxyUrl.current = value;
                } else if (key === SettingKey.StatsSaveInterval) {
                    initialStatsSaveInterval.current = value;
                } else if (key === SettingKey.RelayStreamKeepaliveIntervalSeconds) {
                    initialStreamKeepaliveInterval.current = value;
                } else if (key === SettingKey.RelayStreamDataIntervalTimeoutSeconds) {
                    initialStreamDataTimeoutInterval.current = value;
                } else if (key === SettingKey.ResponsesSessionTTLSeconds) {
                    initialResponsesSessionTTL.current = value;
                } else if (key === SettingKey.CORSAllowOrigins) {
                    initialCorsAllowOrigins.current = value;
                } else if (key === SettingKey.UpstreamErrorBodyMode) {
                    initialUpstreamErrorBodyMode.current = value;
                } else if (key === SettingKey.UpstreamErrorCustomMessage) {
                    initialUpstreamErrorCustomMessage.current = value;
                } else if (key === SettingKey.UpstreamErrorPublicCode) {
                    initialUpstreamErrorPublicCode.current = value;
                } else if (key === SettingKey.CheckInRewardMode) {
                    initialCheckInRewardMode.current = value;
                } else if (key === SettingKey.CheckInRewardAmount) {
                    initialCheckInRewardAmount.current = value;
                } else if (key === SettingKey.CheckInRewardMin) {
                    initialCheckInRewardMin.current = value;
                } else if (key === SettingKey.CheckInRewardMax) {
                    initialCheckInRewardMax.current = value;
                } else if (key === SettingKey.EmailSMTPHost) {
                    initialEmailSMTPHost.current = value;
                } else if (key === SettingKey.EmailSMTPPort) {
                    initialEmailSMTPPort.current = value;
                } else if (key === SettingKey.EmailSMTPUser) {
                    initialEmailSMTPUser.current = value;
                } else if (key === SettingKey.EmailSMTPPassword) {
                    initialEmailSMTPPassword.current = value;
                } else if (key === SettingKey.EmailSMTPFrom) {
                    initialEmailSMTPFrom.current = value;
                } else if (key === SettingKey.EmailSMTPFromName) {
                    initialEmailSMTPFromName.current = value;
                } else if (key === SettingKey.EmailProvider) {
                    initialEmailProvider.current = value;
                } else if (key === SettingKey.EmailHTTPBaseURL) {
                    initialEmailHTTPBaseURL.current = value;
                } else if (key === SettingKey.EmailHTTPFrom) {
                    initialEmailHTTPFrom.current = value;
                } else if (key === SettingKey.EmailHTTPAdminAuth) {
                    initialEmailHTTPAdminAuth.current = value;
                } else if (key === SettingKey.EmailHTTPSiteAuth) {
                    initialEmailHTTPSiteAuth.current = value;
                } else if (key === SettingKey.ClaudeHeaderUserAgent) {
                    initialClaudeHeaderUserAgent.current = value;
                } else if (key === SettingKey.ClaudeHeaderPackageVersion) {
                    initialClaudeHeaderPackageVersion.current = value;
                } else if (key === SettingKey.ClaudeHeaderRuntimeVersion) {
                    initialClaudeHeaderRuntimeVersion.current = value;
                } else if (key === SettingKey.ClaudeHeaderOS) {
                    initialClaudeHeaderOS.current = value;
                } else if (key === SettingKey.ClaudeHeaderArch) {
                    initialClaudeHeaderArch.current = value;
                } else if (key === SettingKey.ClaudeHeaderTimeout) {
                    initialClaudeHeaderTimeout.current = value;
                } else if (key === SettingKey.ClaudeCLIReasoningEffort) {
                    initialClaudeCLIReasoningEffort.current = value;
                } else if (key === SettingKey.ClaudeBetaStripFlags) {
                    initialClaudeBetaStripFlags.current = value;
                } else if (key === SettingKey.CodexHeaderUserAgent) {
                    initialCodexHeaderUserAgent.current = value;
                } else if (key === SettingKey.CodexHeaderBetaFeatures) {
                    initialCodexHeaderBetaFeatures.current = value;
                }
            }
        });
    };

    const handleStreamKeepaliveIntervalBlur = () => {
        const rawValue = streamKeepaliveInterval.trim();
        const numericValue = Number(rawValue);
        if (!rawValue || !Number.isInteger(numericValue) || numericValue < 0) {
            setStreamKeepaliveInterval(initialStreamKeepaliveInterval.current || '15');
            toast.error(t('streamKeepalive.invalid'));
            return;
        }
        const normalizedValue = String(numericValue);
        setStreamKeepaliveInterval(normalizedValue);
        handleSave(
            SettingKey.RelayStreamKeepaliveIntervalSeconds,
            normalizedValue,
            initialStreamKeepaliveInterval.current
        );
    };

    const handleStreamDataTimeoutIntervalBlur = () => {
        const rawValue = streamDataTimeoutInterval.trim();
        const numericValue = Number(rawValue);
        if (!rawValue || !Number.isInteger(numericValue) || numericValue < 0) {
            setStreamDataTimeoutInterval(initialStreamDataTimeoutInterval.current || '900');
            toast.error(t('streamDataTimeout.invalid'));
            return;
        }
        const normalizedValue = String(numericValue);
        setStreamDataTimeoutInterval(normalizedValue);
        handleSave(
            SettingKey.RelayStreamDataIntervalTimeoutSeconds,
            normalizedValue,
            initialStreamDataTimeoutInterval.current
        );
    };

    const handleResponsesSessionTTLBlur = () => {
        const rawValue = responsesSessionTTL.trim();
        const numericValue = Number(rawValue);
        if (!rawValue || !Number.isInteger(numericValue) || numericValue < 0) {
            setResponsesSessionTTL(initialResponsesSessionTTL.current || '3600');
            toast.error(t('responsesSessionTTL.invalid'));
            return;
        }
        const normalizedValue = String(numericValue);
        setResponsesSessionTTL(normalizedValue);
        handleSave(
            SettingKey.ResponsesSessionTTLSeconds,
            normalizedValue,
            initialResponsesSessionTTL.current
        );
    };

    const handleAnthropicAutoCacheControlChange = (checked: boolean) => {
        setAnthropicAutoCacheControl(checked);
        if (checked === initialAnthropicAutoCacheControl.current) return;

        setSetting.mutate({ key: SettingKey.AnthropicAutoCacheControl, value: checked ? 'true' : 'false' }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialAnthropicAutoCacheControl.current = checked;
            }
        });
    };

    const handleOpenAIAutoPromptCacheKeyChange = (checked: boolean) => {
        setOpenAIAutoPromptCacheKey(checked);
        if (checked === initialOpenAIAutoPromptCacheKey.current) return;

        setSetting.mutate({ key: SettingKey.OpenAIAutoPromptCacheKey, value: checked ? 'true' : 'false' }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialOpenAIAutoPromptCacheKey.current = checked;
            }
        });
    };

    const handleClaudeCLIAutoCompactChange = (checked: boolean) => {
        setClaudeCLIAutoCompact(checked);
        if (checked === initialClaudeCLIAutoCompact.current) return;

        setSetting.mutate({ key: SettingKey.ClaudeCLIAutoCompact, value: checked ? 'true' : 'false' }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialClaudeCLIAutoCompact.current = checked;
            }
        });
    };

    const handleCodexFastModeChange = (checked: boolean) => {
        setCodexFastMode(checked);
        if (checked === initialCodexFastMode.current) return;

        setSetting.mutate({ key: SettingKey.CodexFastMode, value: checked ? 'true' : 'false' }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialCodexFastMode.current = checked;
            }
        });
    };

    const handleUserRegistrationEnabledChange = (checked: boolean) => {
        setUserRegistrationEnabled(checked);
        if (checked === initialUserRegistrationEnabled.current) return;

        setSetting.mutate({ key: SettingKey.UserRegistrationEnabled, value: checked ? 'true' : 'false' }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialUserRegistrationEnabled.current = checked;
            }
        });
    };

    const handleEmailVerificationEnabledChange = (checked: boolean) => {
        setEmailVerificationEnabled(checked);
        if (checked === initialEmailVerificationEnabled.current) return;

        setSetting.mutate({ key: SettingKey.EmailVerificationEnabled, value: checked ? 'true' : 'false' }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialEmailVerificationEnabled.current = checked;
            }
        });
    };

    const handleEmailSMTPSSLChange = (checked: boolean) => {
        setEmailSMTPSSL(checked);
        if (checked === initialEmailSMTPSSL.current) return;

        setSetting.mutate({ key: SettingKey.EmailSMTPSSL, value: checked ? 'true' : 'false' }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialEmailSMTPSSL.current = checked;
            }
        });
    };

    const handleUpstreamErrorStatusPassthroughChange = (checked: boolean) => {
        setUpstreamErrorStatusPassthrough(checked);
        if (checked === initialUpstreamErrorStatusPassthrough.current) return;

        setSetting.mutate({ key: SettingKey.UpstreamErrorStatusPassthrough, value: checked ? 'true' : 'false' }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialUpstreamErrorStatusPassthrough.current = checked;
            }
        });
    };

    const handleCheckInEnabledChange = (checked: boolean) => {
        setCheckInEnabled(checked);
        if (checked === initialCheckInEnabled.current) return;

        setSetting.mutate({ key: SettingKey.CheckInEnabled, value: checked ? 'true' : 'false' }, {
            onSuccess: () => {
                toast.success(t('saved'));
                initialCheckInEnabled.current = checked;
            }
        });
    };

    const corsAllowOriginsList = useMemo(() => {
        const value = corsAllowOrigins.trim();
        if (!value) return [];
        if (value === '*') return ['*'];
        return Array.from(new Set(
            value
                .split(/[,\n，]/)
                .map(item => item.trim())
                .filter(Boolean)
        ));
    }, [corsAllowOrigins]);

    const corsAllowOriginsDisplay = useMemo(
        () => (corsAllowOriginsList.length > 0 ? corsAllowOriginsList.join(', ') : t('corsAllowOrigins.hint')),
        [corsAllowOriginsList, t]
    );

    const saveCorsAllowOrigins = (origins: string[]) => {
        const normalizedOrigins = Array.from(new Set(
            origins
                .map(origin => origin.trim())
                .filter(Boolean)
        ));
        const normalizedValue = normalizedOrigins.includes('*') ? '*' : normalizedOrigins.join(',');
        setCorsAllowOrigins(normalizedValue);
        handleSave(SettingKey.CORSAllowOrigins, normalizedValue, initialCorsAllowOrigins.current);
    };

    const handleAddCorsOrigin = () => {
        const newOrigins = Array.from(new Set(
            corsInputValue
                .split(/[,\n，]/)
                .map(item => item.trim())
                .filter(Boolean)
        ));
        if (newOrigins.length === 0) return;

        if (newOrigins.includes('*')) {
            saveCorsAllowOrigins(['*']);
            setCorsInputValue('');
            return;
        }

        const base = corsAllowOriginsList.includes('*') ? [] : corsAllowOriginsList;
        const merged = Array.from(new Set([...base, ...newOrigins]));
        saveCorsAllowOrigins(merged);
        setCorsInputValue('');
    };

    const handleRemoveCorsOrigin = (originToRemove: string) => {
        const nextOrigins = corsAllowOriginsList.filter(origin => origin !== originToRemove);
        saveCorsAllowOrigins(nextOrigins);
    };

    const claudeHeaderRows = useMemo(() => [
        { key: 'dangerousAccess', label: 'Anthropic-Dangerous-Direct-Browser-Access', value: 'true', editable: false },
        { key: 'anthropicVersion', label: 'Anthropic-Version', value: '2023-06-01', editable: false },
        { key: 'userAgent', label: 'User-Agent', value: claudeHeaderUserAgent, editable: true },
        { key: 'app', label: 'X-App', value: 'cli', editable: false },
        { key: 'lang', label: 'X-Stainless-Lang', value: 'js', editable: false },
        { key: 'os', label: 'X-Stainless-OS', value: claudeHeaderOS, editable: claudeHeaderStabilizeDeviceProfile },
        { key: 'arch', label: 'X-Stainless-Arch', value: claudeHeaderArch, editable: claudeHeaderStabilizeDeviceProfile },
        { key: 'package', label: 'X-Stainless-Package-Version', value: claudeHeaderPackageVersion, editable: true },
        { key: 'retry', label: 'X-Stainless-Retry-Count', value: '0', editable: false },
        { key: 'runtime', label: 'X-Stainless-Runtime', value: 'node', editable: false },
        { key: 'runtimeVersion', label: 'X-Stainless-Runtime-Version', value: claudeHeaderRuntimeVersion, editable: true },
        { key: 'timeout', label: 'X-Stainless-Timeout', value: claudeHeaderTimeout, editable: true },
    ], [
        claudeHeaderArch,
        claudeHeaderOS,
        claudeHeaderPackageVersion,
        claudeHeaderRuntimeVersion,
        claudeHeaderStabilizeDeviceProfile,
        claudeHeaderTimeout,
        claudeHeaderUserAgent,
    ]);

    const codexHeaderRows = useMemo(() => [
        { key: 'connection', label: 'Connection', value: 'Keep-Alive', editable: false },
        { key: 'contentType', label: 'Content-Type', value: 'application/json', editable: false },
        { key: 'originator', label: 'Originator', value: 'codex_exec', editable: false },
        { key: 'userAgent', label: 'User-Agent', value: codexHeaderUserAgent, editable: true },
        { key: 'betaFeatures', label: 'X-Codex-Beta-Features', value: codexHeaderBetaFeatures, editable: true },
    ], [codexHeaderBetaFeatures, codexHeaderUserAgent]);

    const openHeaderEditor = (profile: HeaderProfile) => {
        setHeaderDraft({
            claudeUserAgent: claudeHeaderUserAgent,
            claudePackageVersion: claudeHeaderPackageVersion,
            claudeRuntimeVersion: claudeHeaderRuntimeVersion,
            claudeOS: claudeHeaderOS,
            claudeArch: claudeHeaderArch,
            claudeTimeout: claudeHeaderTimeout,
            claudeStabilizeDeviceProfile: claudeHeaderStabilizeDeviceProfile,
            codexUserAgent: codexHeaderUserAgent,
            codexBetaFeatures: codexHeaderBetaFeatures,
        });
        setEditingHeaderProfile(profile);
    };

    const handleSaveHeaderProfile = async () => {
        const updates: Array<{ key: string; value: string; apply: () => void }> = [];

        if (editingHeaderProfile === 'claude') {
            if (headerDraft.claudeUserAgent !== initialClaudeHeaderUserAgent.current) {
                updates.push({
                    key: SettingKey.ClaudeHeaderUserAgent,
                    value: headerDraft.claudeUserAgent,
                    apply: () => {
                        setClaudeHeaderUserAgent(headerDraft.claudeUserAgent);
                        initialClaudeHeaderUserAgent.current = headerDraft.claudeUserAgent;
                    },
                });
            }
            if (headerDraft.claudePackageVersion !== initialClaudeHeaderPackageVersion.current) {
                updates.push({
                    key: SettingKey.ClaudeHeaderPackageVersion,
                    value: headerDraft.claudePackageVersion,
                    apply: () => {
                        setClaudeHeaderPackageVersion(headerDraft.claudePackageVersion);
                        initialClaudeHeaderPackageVersion.current = headerDraft.claudePackageVersion;
                    },
                });
            }
            if (headerDraft.claudeRuntimeVersion !== initialClaudeHeaderRuntimeVersion.current) {
                updates.push({
                    key: SettingKey.ClaudeHeaderRuntimeVersion,
                    value: headerDraft.claudeRuntimeVersion,
                    apply: () => {
                        setClaudeHeaderRuntimeVersion(headerDraft.claudeRuntimeVersion);
                        initialClaudeHeaderRuntimeVersion.current = headerDraft.claudeRuntimeVersion;
                    },
                });
            }
            if (headerDraft.claudeOS !== initialClaudeHeaderOS.current) {
                updates.push({
                    key: SettingKey.ClaudeHeaderOS,
                    value: headerDraft.claudeOS,
                    apply: () => {
                        setClaudeHeaderOS(headerDraft.claudeOS);
                        initialClaudeHeaderOS.current = headerDraft.claudeOS;
                    },
                });
            }
            if (headerDraft.claudeArch !== initialClaudeHeaderArch.current) {
                updates.push({
                    key: SettingKey.ClaudeHeaderArch,
                    value: headerDraft.claudeArch,
                    apply: () => {
                        setClaudeHeaderArch(headerDraft.claudeArch);
                        initialClaudeHeaderArch.current = headerDraft.claudeArch;
                    },
                });
            }
            if (headerDraft.claudeTimeout !== initialClaudeHeaderTimeout.current) {
                updates.push({
                    key: SettingKey.ClaudeHeaderTimeout,
                    value: headerDraft.claudeTimeout,
                    apply: () => {
                        setClaudeHeaderTimeout(headerDraft.claudeTimeout);
                        initialClaudeHeaderTimeout.current = headerDraft.claudeTimeout;
                    },
                });
            }
            if (headerDraft.claudeStabilizeDeviceProfile !== initialClaudeHeaderStabilizeDeviceProfile.current) {
                updates.push({
                    key: SettingKey.ClaudeHeaderStabilizeDeviceProfile,
                    value: headerDraft.claudeStabilizeDeviceProfile ? 'true' : 'false',
                    apply: () => {
                        setClaudeHeaderStabilizeDeviceProfile(headerDraft.claudeStabilizeDeviceProfile);
                        initialClaudeHeaderStabilizeDeviceProfile.current = headerDraft.claudeStabilizeDeviceProfile;
                    },
                });
            }
        } else if (editingHeaderProfile === 'codex') {
            if (headerDraft.codexUserAgent !== initialCodexHeaderUserAgent.current) {
                updates.push({
                    key: SettingKey.CodexHeaderUserAgent,
                    value: headerDraft.codexUserAgent,
                    apply: () => {
                        setCodexHeaderUserAgent(headerDraft.codexUserAgent);
                        initialCodexHeaderUserAgent.current = headerDraft.codexUserAgent;
                    },
                });
            }
            if (headerDraft.codexBetaFeatures !== initialCodexHeaderBetaFeatures.current) {
                updates.push({
                    key: SettingKey.CodexHeaderBetaFeatures,
                    value: headerDraft.codexBetaFeatures,
                    apply: () => {
                        setCodexHeaderBetaFeatures(headerDraft.codexBetaFeatures);
                        initialCodexHeaderBetaFeatures.current = headerDraft.codexBetaFeatures;
                    },
                });
            }
        }

        if (updates.length === 0) {
            setEditingHeaderProfile(null);
            return;
        }

        try {
            await Promise.all(updates.map((update) => setSetting.mutateAsync({ key: update.key, value: update.value })));
            updates.forEach((update) => update.apply());
            toast.success(t('saved'));
            setEditingHeaderProfile(null);
        } catch (error) {
            toast.error(error instanceof Error ? error.message : String(error));
        }
    };

    return (
        <div className="min-w-0 space-y-5 rounded-3xl border border-border bg-card p-4 sm:p-6">
            <h2 className="flex min-w-0 items-center gap-2 text-lg font-bold text-card-foreground">
                <Monitor className="h-5 w-5 shrink-0" />
                {t('system')}
            </h2>

            {/* 代理地址 */}
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                <div className="flex min-w-0 items-center gap-3">
                    <Globe className="h-5 w-5 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 text-sm font-medium">{t('proxyUrl.label')}</span>
                </div>
                <Input
                    value={proxyUrl}
                    onChange={(e) => setProxyUrl(e.target.value)}
                    onBlur={() => handleSave('proxy_url', proxyUrl, initialProxyUrl.current)}
                    placeholder={t('proxyUrl.placeholder')}
                    className="w-full rounded-xl sm:w-48"
                />
            </div>

            {/* 统计保存周期 */}
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                <div className="flex min-w-0 items-center gap-3">
                    <Clock className="h-5 w-5 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 text-sm font-medium">{t('statsSaveInterval.label')}</span>
                </div>
                <Input
                    type="number"
                    value={statsSaveInterval}
                    onChange={(e) => setStatsSaveInterval(e.target.value)}
                    onBlur={() => handleSave('stats_save_interval', statsSaveInterval, initialStatsSaveInterval.current)}
                    placeholder={t('statsSaveInterval.placeholder')}
                    className="w-full rounded-xl sm:w-48"
                />
            </div>

            <div className="grid gap-3 lg:grid-cols-2">
                <div className="grid min-w-0 gap-3 rounded-2xl border border-border/70 bg-muted/20 p-3">
                    <div className="flex items-start justify-between gap-4">
                        <div className="flex min-w-0 items-start gap-3">
                            <Zap className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
                            <div className="min-w-0 space-y-1">
                                <div className="flex flex-wrap items-center gap-2">
                                    <span className="text-sm font-semibold">{t('anthropicAutoCacheControl.label')}</span>
                                    <TooltipProvider>
                                        <Tooltip>
                                            <TooltipTrigger asChild>
                                                <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                            </TooltipTrigger>
                                            <TooltipContent className="max-w-xs">
                                                {t('anthropicAutoCacheControl.hint')}
                                            </TooltipContent>
                                        </Tooltip>
                                    </TooltipProvider>
                                </div>
                                <p className="text-xs leading-5 text-muted-foreground">{t('headerDefaults.claudeDescription')}</p>
                            </div>
                        </div>
                        <Switch
                            checked={anthropicAutoCacheControl}
                            onCheckedChange={handleAnthropicAutoCacheControlChange}
                            aria-label={t('anthropicAutoCacheControl.label')}
                            className="shrink-0"
                        />
                    </div>
                    <div className="grid gap-1.5 border-t border-border/60 pt-3">
                        {claudeHeaderRows.slice(0, 5).map((header) => (
                            <div key={header.key} className="grid min-w-0 gap-1 rounded-xl border border-border/50 bg-background/45 px-2.5 py-2 text-xs sm:grid-cols-[minmax(7.5rem,0.8fr)_minmax(0,1.2fr)] sm:gap-2 sm:border-0 sm:bg-transparent sm:px-0 sm:py-0">
                                <span className="min-w-0 break-all font-mono text-muted-foreground sm:truncate">{header.label}</span>
                                <span className="min-w-0 break-all font-mono text-foreground sm:truncate">{header.value}</span>
                            </div>
                        ))}
                    </div>
                    <div className="grid gap-3 border-t border-border/60 pt-3">
                        <div className="flex items-start justify-between gap-3">
                            <div className="min-w-0 space-y-1">
                                <div className="flex flex-wrap items-center gap-2">
                                    <span className="text-sm font-medium">{t('claudeCLIAutoCompact.label')}</span>
                                    <TooltipProvider>
                                        <Tooltip>
                                            <TooltipTrigger asChild>
                                                <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                            </TooltipTrigger>
                                            <TooltipContent className="max-w-xs">
                                                {t('claudeCLIAutoCompact.hint')}
                                            </TooltipContent>
                                        </Tooltip>
                                    </TooltipProvider>
                                </div>
                            </div>
                            <Switch
                                checked={claudeCLIAutoCompact}
                                onCheckedChange={handleClaudeCLIAutoCompactChange}
                                aria-label={t('claudeCLIAutoCompact.label')}
                                className="shrink-0"
                            />
                        </div>
                        <div className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_9rem] sm:items-center">
                            <div className="min-w-0 space-y-1">
                                <div className="flex flex-wrap items-center gap-2">
                                    <span className="text-sm font-medium">{t('claudeCLIReasoningEffort.label')}</span>
                                    <TooltipProvider>
                                        <Tooltip>
                                            <TooltipTrigger asChild>
                                                <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                            </TooltipTrigger>
                                            <TooltipContent className="max-w-xs">
                                                {t('claudeCLIReasoningEffort.hint')}
                                            </TooltipContent>
                                        </Tooltip>
                                    </TooltipProvider>
                                </div>
                            </div>
                            <select
                                value={claudeCLIReasoningEffort}
                                onChange={(event) => {
                                    const value = event.target.value;
                                    setClaudeCLIReasoningEffort(value);
                                    handleSave(SettingKey.ClaudeCLIReasoningEffort, value, initialClaudeCLIReasoningEffort.current);
                                }}
                                aria-label={t('claudeCLIReasoningEffort.label')}
                                className="h-9 w-full min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                            >
                                <option value="auto">{t('claudeCLIReasoningEffort.auto')}</option>
                                <option value="off">{t('claudeCLIReasoningEffort.off')}</option>
                                <option value="low">{t('claudeCLIReasoningEffort.low')}</option>
                                <option value="medium">{t('claudeCLIReasoningEffort.medium')}</option>
                                <option value="high">{t('claudeCLIReasoningEffort.high')}</option>
                            </select>
                        </div>
                    </div>
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() => openHeaderEditor('claude')}
                        className="h-8 w-fit rounded-xl"
                    >
                        <Pencil className="size-3.5" />
                        {t('headerDefaults.edit')}
                    </Button>
                </div>

                <div className="grid min-w-0 gap-3 rounded-2xl border border-border/70 bg-muted/20 p-3">
                    <div className="flex items-start justify-between gap-4">
                        <div className="flex min-w-0 items-start gap-3">
                            <Fingerprint className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
                            <div className="min-w-0 space-y-1">
                                <div className="flex flex-wrap items-center gap-2">
                                    <span className="text-sm font-semibold">{t('openAIAutoPromptCacheKey.label')}</span>
                                    <TooltipProvider>
                                        <Tooltip>
                                            <TooltipTrigger asChild>
                                                <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                            </TooltipTrigger>
                                            <TooltipContent className="max-w-xs">
                                                {t('openAIAutoPromptCacheKey.hint')}
                                            </TooltipContent>
                                        </Tooltip>
                                    </TooltipProvider>
                                </div>
                                <p className="text-xs leading-5 text-muted-foreground">{t('headerDefaults.codexDescription')}</p>
                            </div>
                        </div>
                        <Switch
                            checked={openAIAutoPromptCacheKey}
                            onCheckedChange={handleOpenAIAutoPromptCacheKeyChange}
                            aria-label={t('openAIAutoPromptCacheKey.label')}
                            className="shrink-0"
                        />
                    </div>
                    <div className="grid gap-1.5 border-t border-border/60 pt-3">
                        {codexHeaderRows.map((header) => (
                            <div key={header.key} className="grid min-w-0 gap-1 rounded-xl border border-border/50 bg-background/45 px-2.5 py-2 text-xs sm:grid-cols-[minmax(7.5rem,0.8fr)_minmax(0,1.2fr)] sm:gap-2 sm:border-0 sm:bg-transparent sm:px-0 sm:py-0">
                                <span className="min-w-0 break-all font-mono text-muted-foreground sm:truncate">{header.label}</span>
                                <span className="min-w-0 break-all font-mono text-foreground sm:truncate">{header.value}</span>
                            </div>
                        ))}
                    </div>
                    <div className="flex items-start justify-between gap-3 border-t border-border/60 pt-3">
                        <div className="min-w-0 space-y-1">
                            <div className="flex flex-wrap items-center gap-2">
                                <span className="text-sm font-medium">{t('codexFastMode.label')}</span>
                                <TooltipProvider>
                                    <Tooltip>
                                        <TooltipTrigger asChild>
                                            <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                        </TooltipTrigger>
                                        <TooltipContent className="max-w-xs">
                                            {t('codexFastMode.hint')}
                                        </TooltipContent>
                                    </Tooltip>
                                </TooltipProvider>
                            </div>
                        </div>
                        <Switch
                            checked={codexFastMode}
                            onCheckedChange={handleCodexFastModeChange}
                            aria-label={t('codexFastMode.label')}
                            className="shrink-0"
                        />
                    </div>
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() => openHeaderEditor('codex')}
                        className="h-8 w-fit rounded-xl"
                    >
                        <Pencil className="size-3.5" />
                        {t('headerDefaults.edit')}
                    </Button>
                </div>
            </div>

            {/* Claude Beta 剥离标记（anyrouter 抽风逃生） */}
            <div className="flex flex-col gap-3 rounded-2xl border border-border/70 bg-muted/20 p-3 sm:flex-row sm:items-start sm:justify-between">
                <div className="flex min-w-0 items-start gap-3">
                    <AlertTriangle className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
                    <div className="min-w-0 space-y-1">
                        <div className="flex flex-wrap items-center gap-2">
                            <span className="text-sm font-medium">{t('claudeBetaStripFlags.label')}</span>
                            <span className="rounded-full bg-background px-2 py-0.5 text-[11px] text-muted-foreground">
                                {t('claudeBetaStripFlags.defaultHint')}
                            </span>
                            <TooltipProvider>
                                <Tooltip>
                                    <TooltipTrigger asChild>
                                        <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                    </TooltipTrigger>
                                    <TooltipContent className="max-w-xs">
                                        {t('claudeBetaStripFlags.hint')}
                                    </TooltipContent>
                                </Tooltip>
                            </TooltipProvider>
                        </div>
                        <p className="text-xs leading-5 text-muted-foreground">
                            {t('claudeBetaStripFlags.description')}
                        </p>
                    </div>
                </div>
                <Input
                    value={claudeBetaStripFlags}
                    onChange={(event) => setClaudeBetaStripFlags(event.target.value)}
                    onBlur={() => handleSave(
                        SettingKey.ClaudeBetaStripFlags,
                        claudeBetaStripFlags,
                        initialClaudeBetaStripFlags.current
                    )}
                    placeholder={t('claudeBetaStripFlags.placeholder')}
                    aria-label={t('claudeBetaStripFlags.label')}
                    className="w-full min-w-0 rounded-xl font-mono text-xs sm:w-80"
                />
            </div>

            <Dialog open={editingHeaderProfile !== null} onOpenChange={(open) => {
                if (!open) setEditingHeaderProfile(null);
            }}>
                <DialogContent className="max-h-[calc(100dvh-1rem)] w-[calc(100vw-1rem)] max-w-[calc(100vw-1rem)] overflow-hidden rounded-3xl border-border bg-card p-0 sm:max-w-3xl">
                    <DialogHeader className="border-b border-border/60 px-5 py-4 text-left">
                        <DialogTitle className="flex items-center gap-2 text-lg">
                            <Fingerprint className="size-5" />
                            {editingHeaderProfile === 'claude' ? t('headerDefaults.claude') : t('headerDefaults.codex')}
                        </DialogTitle>
                        <DialogDescription className="text-sm">
                            {t('headerDefaults.hint')}
                        </DialogDescription>
                    </DialogHeader>
                    <div className="max-h-[min(70dvh,34rem)] overflow-y-auto px-5 py-4">
                        {editingHeaderProfile === 'claude' ? (
                            <div className="grid gap-3">
                                <div className="flex items-center justify-between gap-3 rounded-2xl border border-border bg-background/70 p-3">
                                    <div className="min-w-0">
                                        <div className="text-sm font-medium">{t('headerDefaults.stabilizeDeviceProfile')}</div>
                                        <div className="text-xs text-muted-foreground">X-Stainless-OS / X-Stainless-Arch</div>
                                    </div>
                                    <Switch
                                        checked={headerDraft.claudeStabilizeDeviceProfile}
                                        onCheckedChange={(checked) => setHeaderDraft((draft) => ({ ...draft, claudeStabilizeDeviceProfile: checked }))}
                                    />
                                </div>
                                {[
                                    { label: 'Anthropic-Dangerous-Direct-Browser-Access', value: 'true', fixed: true },
                                    { label: 'Anthropic-Version', value: '2023-06-01', fixed: true },
                                    { label: 'User-Agent', value: headerDraft.claudeUserAgent, onChange: (value: string) => setHeaderDraft((draft) => ({ ...draft, claudeUserAgent: value })) },
                                    { label: 'X-App', value: 'cli', fixed: true },
                                    { label: 'X-Stainless-Lang', value: 'js', fixed: true },
                                    { label: 'X-Stainless-OS', value: headerDraft.claudeOS, onChange: (value: string) => setHeaderDraft((draft) => ({ ...draft, claudeOS: value })) },
                                    { label: 'X-Stainless-Arch', value: headerDraft.claudeArch, onChange: (value: string) => setHeaderDraft((draft) => ({ ...draft, claudeArch: value })) },
                                    { label: 'X-Stainless-Package-Version', value: headerDraft.claudePackageVersion, onChange: (value: string) => setHeaderDraft((draft) => ({ ...draft, claudePackageVersion: value })) },
                                    { label: 'X-Stainless-Retry-Count', value: '0', fixed: true },
                                    { label: 'X-Stainless-Runtime', value: 'node', fixed: true },
                                    { label: 'X-Stainless-Runtime-Version', value: headerDraft.claudeRuntimeVersion, onChange: (value: string) => setHeaderDraft((draft) => ({ ...draft, claudeRuntimeVersion: value })) },
                                    { label: 'X-Stainless-Timeout', value: headerDraft.claudeTimeout, onChange: (value: string) => setHeaderDraft((draft) => ({ ...draft, claudeTimeout: value })) },
                                ].map((header) => (
                                    <div key={header.label} className="grid min-w-0 gap-2 rounded-2xl border border-border bg-background/70 p-3 md:grid-cols-[minmax(12rem,0.8fr)_minmax(0,1.2fr)_auto] md:items-center">
                                        <span className="min-w-0 break-all font-mono text-xs text-muted-foreground">{header.label}</span>
                                        {header.fixed ? (
                                            <span className="min-w-0 break-all rounded-xl bg-muted/50 px-3 py-2 font-mono text-xs text-foreground">{header.value}</span>
                                        ) : (
                                            <Input
                                                value={header.value}
                                                onChange={(event) => header.onChange?.(event.target.value)}
                                                className="min-w-0 rounded-xl font-mono text-xs"
                                            />
                                        )}
                                        <span className="w-fit rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">
                                            {header.fixed ? t('headerDefaults.fixed') : t('headerDefaults.editable')}
                                        </span>
                                    </div>
                                ))}
                            </div>
                        ) : (
                            <div className="grid gap-3">
                                {[
                                    { label: 'Connection', value: 'Keep-Alive', fixed: true },
                                    { label: 'Content-Type', value: 'application/json', fixed: true },
                                    { label: 'Originator', value: 'codex_exec', fixed: true },
                                    { label: 'User-Agent', value: headerDraft.codexUserAgent, onChange: (value: string) => setHeaderDraft((draft) => ({ ...draft, codexUserAgent: value })) },
                                    { label: 'X-Codex-Beta-Features', value: headerDraft.codexBetaFeatures, onChange: (value: string) => setHeaderDraft((draft) => ({ ...draft, codexBetaFeatures: value })) },
                                ].map((header) => (
                                    <div key={header.label} className="grid min-w-0 gap-2 rounded-2xl border border-border bg-background/70 p-3 md:grid-cols-[minmax(12rem,0.8fr)_minmax(0,1.2fr)_auto] md:items-center">
                                        <span className="min-w-0 break-all font-mono text-xs text-muted-foreground">{header.label}</span>
                                        {header.fixed ? (
                                            <span className="min-w-0 break-all rounded-xl bg-muted/50 px-3 py-2 font-mono text-xs text-foreground">{header.value}</span>
                                        ) : (
                                            <Input
                                                value={header.value}
                                                onChange={(event) => header.onChange?.(event.target.value)}
                                                className="min-w-0 rounded-xl font-mono text-xs"
                                            />
                                        )}
                                        <span className="w-fit rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">
                                            {header.fixed ? t('headerDefaults.fixed') : t('headerDefaults.editable')}
                                        </span>
                                    </div>
                                ))}
                            </div>
                        )}
                    </div>
                    <DialogFooter className="flex-col-reverse gap-2 border-t border-border/60 px-5 py-4 sm:flex-row">
                        <Button
                            type="button"
                            variant="outline"
                            onClick={() => setEditingHeaderProfile(null)}
                            className="w-full rounded-xl sm:w-auto"
                        >
                            <X className="size-4" />
                            {t('headerDefaults.cancel')}
                        </Button>
                        <Button
                            type="button"
                            onClick={handleSaveHeaderProfile}
                            disabled={setSetting.isPending}
                            className="w-full rounded-xl sm:w-auto"
                        >
                            {setSetting.isPending ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}
                            {t('headerDefaults.save')}
                        </Button>
                    </DialogFooter>
                </DialogContent>
            </Dialog>

            <div className="grid gap-3 rounded-2xl border border-border/70 bg-muted/20 p-3">
                <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
                    <div className="flex min-w-0 items-start gap-3">
                        <Radio className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
                        <div className="min-w-0 space-y-1">
                            <div className="flex flex-wrap items-center gap-2">
                                <span className="text-sm font-medium">{t('streamKeepalive.label')}</span>
                                <span className="rounded-full bg-background px-2 py-0.5 text-[11px] text-muted-foreground">
                                    {t('streamKeepalive.disabledHint')}
                                </span>
                                <TooltipProvider>
                                    <Tooltip>
                                        <TooltipTrigger asChild>
                                            <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                        </TooltipTrigger>
                                        <TooltipContent className="max-w-xs">
                                            {t('streamKeepalive.hint')}
                                        </TooltipContent>
                                    </Tooltip>
                                </TooltipProvider>
                            </div>
                            <p className="text-xs leading-5 text-muted-foreground">
                                {t('streamKeepalive.description')}
                            </p>
                            <p className="break-all text-[11px] leading-5 text-muted-foreground">
                                <span className="font-mono">OCTOPUS_RELAY_STREAM_KEEPALIVE_INTERVAL_SECONDS</span>
                                {' '}
                                {t('streamKeepalive.envHint')}
                            </p>
                        </div>
                    </div>
                    <div className="flex w-full min-w-0 items-center gap-2 self-start sm:w-auto sm:shrink-0 sm:self-center">
                        <Input
                            type="number"
                            min="0"
                            step="1"
                            inputMode="numeric"
                            value={streamKeepaliveInterval}
                            onChange={(event) => setStreamKeepaliveInterval(event.target.value)}
                            onBlur={handleStreamKeepaliveIntervalBlur}
                            placeholder={t('streamKeepalive.placeholder')}
                            aria-label={t('streamKeepalive.label')}
                            className="w-24 rounded-xl sm:w-28"
                        />
                        <span className="min-w-0 text-xs text-muted-foreground">{t('streamKeepalive.seconds')}</span>
                    </div>
                </div>
                <div className="flex flex-col gap-3 border-t border-border/60 pt-3 sm:flex-row sm:items-start sm:justify-between">
                    <div className="flex min-w-0 items-start gap-3">
                        <Clock className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
                        <div className="min-w-0 space-y-1">
                            <div className="flex flex-wrap items-center gap-2">
                                <span className="text-sm font-medium">{t('streamDataTimeout.label')}</span>
                                <span className="rounded-full bg-background px-2 py-0.5 text-[11px] text-muted-foreground">
                                    {t('streamDataTimeout.disabledHint')}
                                </span>
                                <TooltipProvider>
                                    <Tooltip>
                                        <TooltipTrigger asChild>
                                            <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                        </TooltipTrigger>
                                        <TooltipContent className="max-w-xs">
                                            {t('streamDataTimeout.hint')}
                                        </TooltipContent>
                                    </Tooltip>
                                </TooltipProvider>
                            </div>
                            <p className="text-xs leading-5 text-muted-foreground">
                                {t('streamDataTimeout.description')}
                            </p>
                            <p className="break-all text-[11px] leading-5 text-muted-foreground">
                                <span className="font-mono">OCTOPUS_RELAY_STREAM_DATA_INTERVAL_TIMEOUT_SECONDS</span>
                                {' '}
                                {t('streamDataTimeout.envHint')}
                            </p>
                        </div>
                    </div>
                    <div className="flex w-full min-w-0 items-center gap-2 self-start sm:w-auto sm:shrink-0 sm:self-center">
                        <Input
                            type="number"
                            min="0"
                            step="1"
                            inputMode="numeric"
                            value={streamDataTimeoutInterval}
                            onChange={(event) => setStreamDataTimeoutInterval(event.target.value)}
                            onBlur={handleStreamDataTimeoutIntervalBlur}
                            placeholder={t('streamDataTimeout.placeholder')}
                            aria-label={t('streamDataTimeout.label')}
                            className="w-24 rounded-xl sm:w-28"
                        />
                        <span className="min-w-0 text-xs text-muted-foreground">{t('streamDataTimeout.seconds')}</span>
                    </div>
                </div>
                <div className="flex flex-col gap-3 border-t border-border/60 pt-3 sm:flex-row sm:items-start sm:justify-between">
                    <div className="flex min-w-0 items-start gap-3">
                        <Fingerprint className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
                        <div className="min-w-0 space-y-1">
                            <div className="flex flex-wrap items-center gap-2">
                                <span className="text-sm font-medium">{t('responsesSessionTTL.label')}</span>
                                <span className="rounded-full bg-background px-2 py-0.5 text-[11px] text-muted-foreground">
                                    {t('responsesSessionTTL.defaultHint')}
                                </span>
                                <TooltipProvider>
                                    <Tooltip>
                                        <TooltipTrigger asChild>
                                            <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                        </TooltipTrigger>
                                        <TooltipContent className="max-w-xs">
                                            {t('responsesSessionTTL.hint')}
                                        </TooltipContent>
                                    </Tooltip>
                                </TooltipProvider>
                            </div>
                            <p className="text-xs leading-5 text-muted-foreground">
                                {t('responsesSessionTTL.description')}
                            </p>
                        </div>
                    </div>
                    <div className="flex w-full min-w-0 items-center gap-2 self-start sm:w-auto sm:shrink-0 sm:self-center">
                        <Input
                            type="number"
                            min="0"
                            step="1"
                            inputMode="numeric"
                            value={responsesSessionTTL}
                            onChange={(event) => setResponsesSessionTTL(event.target.value)}
                            onBlur={handleResponsesSessionTTLBlur}
                            placeholder={t('responsesSessionTTL.placeholder')}
                            aria-label={t('responsesSessionTTL.label')}
                            className="w-24 rounded-xl sm:w-28"
                        />
                        <span className="min-w-0 text-xs text-muted-foreground">{t('responsesSessionTTL.seconds')}</span>
                    </div>
                </div>
            </div>

            {/* 新用户直接注册 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex min-w-0 items-center gap-3">
                    <UserPlus className="h-5 w-5 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 text-sm font-medium">{t('userRegistration.label')}</span>
                    <TooltipProvider>
                        <Tooltip>
                            <TooltipTrigger asChild>
                                <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                            </TooltipTrigger>
                            <TooltipContent>
                                {t('userRegistration.hint')}
                            </TooltipContent>
                        </Tooltip>
                    </TooltipProvider>
                </div>
                <Switch
                    checked={userRegistrationEnabled}
                    onCheckedChange={handleUserRegistrationEnabledChange}
                    aria-label={t('userRegistration.label')}
                    className="shrink-0"
                />
            </div>

            {/* 注册邮箱验证 */}
            <div className="grid gap-3 rounded-2xl border border-border/70 bg-muted/20 p-3">
                <div className="flex items-center justify-between gap-4">
                    <div className="flex min-w-0 items-center gap-3">
                        <Mail className="h-5 w-5 shrink-0 text-muted-foreground" />
                        <span className="min-w-0 text-sm font-medium">{t('emailVerification.title')}</span>
                        <TooltipProvider>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                </TooltipTrigger>
                                <TooltipContent>
                                    {t('emailVerification.hint')}
                                </TooltipContent>
                            </Tooltip>
                        </TooltipProvider>
                    </div>
                    <Switch
                        checked={emailVerificationEnabled}
                        onCheckedChange={handleEmailVerificationEnabledChange}
                        aria-label={t('emailVerification.enabled')}
                        className="shrink-0"
                    />
                </div>
                <div className="grid gap-2 border-t border-border/60 pt-3 sm:grid-cols-[180px_minmax(0,1fr)] sm:items-center">
                    <div className="flex min-w-0 items-center gap-2">
                        <span className="min-w-0 text-sm font-medium">{t('emailVerification.provider')}</span>
                        <TooltipProvider>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                </TooltipTrigger>
                                <TooltipContent className="max-w-xs">
                                    {t('emailVerification.providerHint')}
                                </TooltipContent>
                            </Tooltip>
                        </TooltipProvider>
                    </div>
                    <select
                        value={emailProvider}
                        onChange={(event) => {
                            const value = event.target.value;
                            setEmailProvider(value);
                            handleSave(SettingKey.EmailProvider, value, initialEmailProvider.current);
                        }}
                        aria-label={t('emailVerification.provider')}
                        className="h-9 w-full min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                    >
                        <option value="smtp">{t('emailVerification.providerSMTP')}</option>
                        <option value="http">{t('emailVerification.providerHTTP')}</option>
                    </select>
                </div>
                {emailProvider === 'smtp' && (
                <div className="grid gap-2 sm:grid-cols-2">
                    <Input
                        value={emailSMTPHost}
                        onChange={(event) => setEmailSMTPHost(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.EmailSMTPHost,
                            emailSMTPHost,
                            initialEmailSMTPHost.current
                        )}
                        placeholder={t('emailVerification.hostPlaceholder')}
                        aria-label={t('emailVerification.host')}
                        className="rounded-xl"
                    />
                    <Input
                        type="number"
                        min="0"
                        value={emailSMTPPort}
                        onChange={(event) => setEmailSMTPPort(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.EmailSMTPPort,
                            emailSMTPPort,
                            initialEmailSMTPPort.current
                        )}
                        placeholder={t('emailVerification.portPlaceholder')}
                        aria-label={t('emailVerification.port')}
                        className="rounded-xl"
                    />
                    <Input
                        value={emailSMTPUser}
                        onChange={(event) => setEmailSMTPUser(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.EmailSMTPUser,
                            emailSMTPUser,
                            initialEmailSMTPUser.current
                        )}
                        placeholder={t('emailVerification.userPlaceholder')}
                        aria-label={t('emailVerification.user')}
                        className="rounded-xl"
                    />
                    <Input
                        type="password"
                        value={emailSMTPPassword}
                        onChange={(event) => setEmailSMTPPassword(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.EmailSMTPPassword,
                            emailSMTPPassword,
                            initialEmailSMTPPassword.current
                        )}
                        placeholder={initialEmailSMTPPassword.current === SECRET_MASK ? t('emailVerification.passwordKeptPlaceholder') : t('emailVerification.passwordPlaceholder')}
                        aria-label={t('emailVerification.password')}
                        autoComplete="new-password"
                        className="rounded-xl"
                    />
                    <Input
                        value={emailSMTPFrom}
                        onChange={(event) => setEmailSMTPFrom(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.EmailSMTPFrom,
                            emailSMTPFrom,
                            initialEmailSMTPFrom.current
                        )}
                        placeholder={t('emailVerification.fromPlaceholder')}
                        aria-label={t('emailVerification.from')}
                        className="rounded-xl"
                    />
                    <Input
                        value={emailSMTPFromName}
                        onChange={(event) => setEmailSMTPFromName(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.EmailSMTPFromName,
                            emailSMTPFromName,
                            initialEmailSMTPFromName.current
                        )}
                        placeholder={t('emailVerification.fromNamePlaceholder')}
                        aria-label={t('emailVerification.fromName')}
                        className="rounded-xl"
                    />
                </div>
                )}
                {emailProvider === 'smtp' && (
                <div className="flex items-center justify-between gap-4 border-t border-border/60 pt-3">
                    <div className="flex min-w-0 items-center gap-3">
                        <span className="min-w-0 text-sm font-medium">{t('emailVerification.ssl')}</span>
                        <TooltipProvider>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                </TooltipTrigger>
                                <TooltipContent>
                                    {t('emailVerification.sslHint')}
                                </TooltipContent>
                            </Tooltip>
                        </TooltipProvider>
                    </div>
                    <Switch
                        checked={emailSMTPSSL}
                        onCheckedChange={handleEmailSMTPSSLChange}
                        aria-label={t('emailVerification.ssl')}
                        className="shrink-0"
                    />
                </div>
                )}
                {emailProvider === 'http' && (
                <div className="grid gap-2 sm:grid-cols-2">
                    <Input
                        value={emailHTTPBaseURL}
                        onChange={(event) => setEmailHTTPBaseURL(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.EmailHTTPBaseURL,
                            emailHTTPBaseURL,
                            initialEmailHTTPBaseURL.current
                        )}
                        placeholder={t('emailVerification.httpBaseUrlPlaceholder')}
                        aria-label={t('emailVerification.httpBaseUrl')}
                        className="rounded-xl"
                    />
                    <Input
                        value={emailHTTPFrom}
                        onChange={(event) => setEmailHTTPFrom(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.EmailHTTPFrom,
                            emailHTTPFrom,
                            initialEmailHTTPFrom.current
                        )}
                        placeholder={t('emailVerification.httpFromPlaceholder')}
                        aria-label={t('emailVerification.httpFrom')}
                        className="rounded-xl"
                    />
                    <Input
                        type="password"
                        value={emailHTTPAdminAuth}
                        onChange={(event) => setEmailHTTPAdminAuth(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.EmailHTTPAdminAuth,
                            emailHTTPAdminAuth,
                            initialEmailHTTPAdminAuth.current
                        )}
                        placeholder={initialEmailHTTPAdminAuth.current === SECRET_MASK ? t('emailVerification.passwordKeptPlaceholder') : t('emailVerification.httpAdminAuthPlaceholder')}
                        aria-label={t('emailVerification.httpAdminAuth')}
                        autoComplete="new-password"
                        className="rounded-xl"
                    />
                    <Input
                        type="password"
                        value={emailHTTPSiteAuth}
                        onChange={(event) => setEmailHTTPSiteAuth(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.EmailHTTPSiteAuth,
                            emailHTTPSiteAuth,
                            initialEmailHTTPSiteAuth.current
                        )}
                        placeholder={initialEmailHTTPSiteAuth.current === SECRET_MASK ? t('emailVerification.passwordKeptPlaceholder') : t('emailVerification.httpSiteAuthPlaceholder')}
                        aria-label={t('emailVerification.httpSiteAuth')}
                        autoComplete="new-password"
                        className="rounded-xl"
                    />
                </div>
                )}
            </div>

            {/* 每日签到 */}
            <div className="grid gap-3 rounded-2xl border border-border/70 bg-muted/20 p-3">
                <div className="flex items-center justify-between gap-4">
                    <div className="flex min-w-0 items-center gap-3">
                        <Gift className="h-5 w-5 shrink-0 text-muted-foreground" />
                        <span className="min-w-0 text-sm font-medium">{t('checkIn.title')}</span>
                        <TooltipProvider>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                </TooltipTrigger>
                                <TooltipContent>
                                    {t('checkIn.hint')}
                                </TooltipContent>
                            </Tooltip>
                        </TooltipProvider>
                    </div>
                    <Switch
                        checked={checkInEnabled}
                        onCheckedChange={handleCheckInEnabledChange}
                        aria-label={t('checkIn.enabled')}
                        className="shrink-0"
                    />
                </div>
                <div className="grid gap-2 sm:grid-cols-[180px_minmax(0,1fr)_minmax(0,1fr)]">
                    <select
                        value={checkInRewardMode}
                        onChange={(event) => {
                            const value = event.target.value;
                            setCheckInRewardMode(value);
                            handleSave(SettingKey.CheckInRewardMode, value, initialCheckInRewardMode.current);
                        }}
                        aria-label={t('checkIn.rewardMode')}
                        className="h-9 w-full min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                    >
                        <option value="fixed">{t('checkIn.fixed')}</option>
                        <option value="random">{t('checkIn.random')}</option>
                    </select>
                    <Input
                        type="number"
                        min="0"
                        value={checkInRewardAmount}
                        onChange={(event) => setCheckInRewardAmount(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.CheckInRewardAmount,
                            checkInRewardAmount,
                            initialCheckInRewardAmount.current
                        )}
                        placeholder={t('checkIn.amountPlaceholder')}
                        disabled={checkInRewardMode !== 'fixed'}
                        className="rounded-xl"
                    />
                    <div className="grid min-w-0 grid-cols-2 gap-2">
                        <Input
                            type="number"
                            min="0"
                            value={checkInRewardMin}
                            onChange={(event) => setCheckInRewardMin(event.target.value)}
                            onBlur={() => handleSave(
                                SettingKey.CheckInRewardMin,
                                checkInRewardMin,
                                initialCheckInRewardMin.current
                            )}
                            placeholder={t('checkIn.minPlaceholder')}
                            disabled={checkInRewardMode !== 'random'}
                            className="rounded-xl"
                        />
                        <Input
                            type="number"
                            min="0"
                            value={checkInRewardMax}
                            onChange={(event) => setCheckInRewardMax(event.target.value)}
                            onBlur={() => handleSave(
                                SettingKey.CheckInRewardMax,
                                checkInRewardMax,
                                initialCheckInRewardMax.current
                            )}
                            placeholder={t('checkIn.maxPlaceholder')}
                            disabled={checkInRewardMode !== 'random'}
                            className="rounded-xl"
                        />
                    </div>
                </div>
            </div>

            {/* 上游错误响应 */}
            <div className="grid gap-3 rounded-2xl border border-border/70 bg-muted/20 p-3">
                <div className="flex items-center justify-between gap-4">
                    <div className="flex min-w-0 items-center gap-3">
                        <AlertTriangle className="h-5 w-5 shrink-0 text-muted-foreground" />
                        <span className="min-w-0 text-sm font-medium">{t('upstreamError.title')}</span>
                        <TooltipProvider>
                            <Tooltip>
                                <TooltipTrigger asChild>
                                    <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                                </TooltipTrigger>
                                <TooltipContent>
                                    {t('upstreamError.hint')}
                                </TooltipContent>
                            </Tooltip>
                        </TooltipProvider>
                    </div>
                    <Switch
                        checked={upstreamErrorStatusPassthrough}
                        onCheckedChange={handleUpstreamErrorStatusPassthroughChange}
                        aria-label={t('upstreamError.statusPassthrough')}
                        className="shrink-0"
                    />
                </div>
                <Input
                    value={upstreamErrorPublicCode}
                    onChange={(event) => setUpstreamErrorPublicCode(event.target.value)}
                    onBlur={() => handleSave(
                        SettingKey.UpstreamErrorPublicCode,
                        upstreamErrorPublicCode,
                        initialUpstreamErrorPublicCode.current
                    )}
                    placeholder={t('upstreamError.publicCodePlaceholder')}
                    aria-label={t('upstreamError.publicCode')}
                    className="rounded-xl"
                />
                <div className="grid gap-2 sm:grid-cols-[180px_minmax(0,1fr)]">
                    <select
                        value={upstreamErrorBodyMode}
                        onChange={(event) => {
                            const value = event.target.value;
                            setUpstreamErrorBodyMode(value);
                            handleSave(SettingKey.UpstreamErrorBodyMode, value, initialUpstreamErrorBodyMode.current);
                        }}
                        aria-label={t('upstreamError.bodyMode')}
                        className="h-9 w-full min-w-0 rounded-xl border border-input bg-background px-3 text-sm text-foreground"
                    >
                        <option value="redacted_upstream">{t('upstreamError.redacted')}</option>
                        <option value="custom_message">{t('upstreamError.custom')}</option>
                        <option value="octopus_standard">{t('upstreamError.standard')}</option>
                    </select>
                    <Input
                        value={upstreamErrorCustomMessage}
                        onChange={(event) => setUpstreamErrorCustomMessage(event.target.value)}
                        onBlur={() => handleSave(
                            SettingKey.UpstreamErrorCustomMessage,
                            upstreamErrorCustomMessage,
                            initialUpstreamErrorCustomMessage.current
                        )}
                        placeholder={t('upstreamError.customPlaceholder')}
                        disabled={upstreamErrorBodyMode !== 'custom_message'}
                        className="rounded-xl"
                    />
                </div>
            </div>

            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                <div className="flex min-w-0 items-center gap-3">
                    <Shield className="h-5 w-5 shrink-0 text-muted-foreground" />
                    <span className="min-w-0 text-sm font-medium">{t('corsAllowOrigins.label')}</span>
                    <TooltipProvider>
                        <Tooltip>
                            <TooltipTrigger asChild>
                                <HelpCircle className="size-4 text-muted-foreground cursor-help" />
                            </TooltipTrigger>
                            <TooltipContent>
                                {t('corsAllowOrigins.hint')}
                                <br />
                                {t('corsAllowOrigins.example')}
                            </TooltipContent>
                        </Tooltip>
                    </TooltipProvider>
                </div>
                <Popover>
                    <PopoverTrigger asChild>
                        <button
                            type="button"
                            className="border-input focus-visible:border-ring focus-visible:ring-ring/50 min-h-9 w-full rounded-xl border bg-transparent px-3 py-2 text-left text-sm shadow-xs outline-none transition-[color,box-shadow] focus-visible:ring-[3px] sm:w-48"
                            title={corsAllowOriginsDisplay}
                        >
                            <span className={`block overflow-hidden text-ellipsis whitespace-nowrap ${corsAllowOriginsList.length === 0 ? 'text-muted-foreground' : ''}`}>
                                {corsAllowOriginsDisplay}
                            </span>
                        </button>
                    </PopoverTrigger>
                    <PopoverContent className="w-[min(18rem,calc(100vw-1rem))] max-w-[calc(100vw-1rem)] space-y-2 rounded-3xl bg-card p-3">
                        <Input
                            value={corsInputValue}
                            onChange={(e) => setCorsInputValue(e.target.value)}
                            onKeyDown={(e) => {
                                if (e.key === 'Enter') {
                                    e.preventDefault();
                                    handleAddCorsOrigin();
                                }
                            }}
                            placeholder={t('corsAllowOrigins.example')}
                            className="h-9 rounded-xl"
                            autoFocus
                        />
                        <div className="max-h-48 space-y-1 overflow-y-auto">
                            {corsAllowOriginsList.length > 0 && (
                                corsAllowOriginsList.map((origin) => (
                                    <div key={origin} className="flex min-w-0 items-center justify-between gap-2 rounded-xl border border-border/60 px-2 py-1">
                                        <span className="min-w-0 break-all text-xs leading-5">{origin}</span>
                                        <button
                                            type="button"
                                            onClick={() => handleRemoveCorsOrigin(origin)}
                                            className="text-muted-foreground transition-colors hover:text-destructive"
                                            aria-label={`remove ${origin}`}
                                        >
                                            <X className="size-4" />
                                        </button>
                                    </div>
                                ))
                            )}
                        </div>
                    </PopoverContent>
                </Popover>
            </div>
        </div>
    );
}
