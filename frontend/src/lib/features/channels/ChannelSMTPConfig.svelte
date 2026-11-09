<script>
  import { t } from '../../stores/i18n.svelte.js';
  import { channels } from '../../api/channels.js';
  import AlertBox from '../../components/AlertBox.svelte';
  import Button from '../../components/Button.svelte';
  import Spinner from '../../components/Spinner.svelte';
  import DescriptionText from '../../components/DescriptionText.svelte';
  import Toggle from '../../components/Toggle.svelte';
  import { isSystemAdmin } from '../../stores/permissions.svelte.js';
	import TextField from '../../components/TextField.svelte';
	import SelectField from '../../components/SelectField.svelte';

  let {
    channelId,
    formData = $bindable({
      host: '',
      port: 587,
      username: '',
      password: '',
      from_email: '',
      from_name: '',
      encryption: 'tls',
      skip_tls_verify: false,
      auth_method: 'auto',
      enabled: false,
      send_provider: 'smtp',
      graph_tenant_id: '',
      graph_client_id: '',
      graph_client_secret: '',
      graph_mailbox_upn: ''
    }),
    onSave = () => {}
  } = $props();

  let testResult = $state(null);
  let testEmail = $state('');
  let loading = $state(false);

  async function testSmtpSettings() {
    if (!$isSystemAdmin || loading) return;
    // Provider-specific required fields first: Graph mode must not save a
    // tenant/client/UPN-less config just because the legacy host gate lets it
    // through, and SMTP mode keeps its own checks.
    const validation = validate();
    if (!validation.valid) {
      testResult = { success: false, message: validation.message };
      return;
    }
    // Graph mode never dials the SMTP host, but the backend's readiness gate
    // still needs host/port/from placeholders — treat them as satisfied and
    // prefill host on demand so the test button is usable from a bare Graph
    // config.
    if (formData.send_provider === 'graph' && !formData.host) {
      formData.host = 'graph.local';
    }
    if (!channelId || !formData.host || !formData.from_email) {
      testResult = { success: false, message: t('channel.smtpHostAndFromRequired') };
      return;
    }

    if (!testEmail) {
      testResult = { success: false, message: t('channel.testEmailRequired') };
      return;
    }

    testResult = { success: true, message: t('channel.sendingTestEmail'), loading: true };
    loading = true;

    try {
      // Save config first
      const configData = getConfig();
      await channels.updateConfig(channelId, configData);

      // Test the channel with test email
      const result = await channels.testWithEmail(channelId, testEmail);
      if (result.success) {
        testResult = {
          success: true,
          message: formData.send_provider === 'graph'
            ? t('channel.testEmailSentViaGraph')
            : t('channel.testEmailSent'),
          loading: false
        };
        onSave();
      } else {
        testResult = {
          success: false,
          message: `${t('channel.testEmailFailed')}: ${result.message || 'Unknown error'}`,
          loading: false
        };
      }
    } catch (error) {
      console.error('Failed to test SMTP:', error);
      testResult = {
        success: false,
        message: t('channel.testEmailFailed') + ': ' + (error.message || error),
        loading: false
      };
    } finally {
      loading = false;
    }
  }

  export function validate() {
    if (formData.send_provider === 'graph') {
      if (!formData.graph_tenant_id?.trim()) {
        return { valid: false, message: t('channel.graphTenantRequired') };
      }
      if (!formData.graph_client_id?.trim()) {
        return { valid: false, message: t('channel.graphClientIdRequired') };
      }
      if (!formData.graph_mailbox_upn?.trim()) {
        return { valid: false, message: t('channel.graphMailboxRequired') };
      }
      if (!formData.from_email?.trim()) {
        return { valid: false, message: t('channel.smtpFromEmailRequired') };
      }
      return { valid: true };
    }
    if (!formData.host?.trim()) {
      return { valid: false, message: t('channel.smtpHostRequired') };
    }
    if (!formData.from_email?.trim()) {
      return { valid: false, message: t('channel.smtpFromEmailRequired') };
    }
    return { valid: true };
  }

  export function getConfig() {
    const plaintext = formData.encryption === 'none';
    if (formData.send_provider === 'graph') {
      return {
        graph_send_provider: 'graph',
        graph_tenant_id: formData.graph_tenant_id,
        graph_client_id: formData.graph_client_id,
        // Blank means "keep the saved secret" (partial-merge contract).
        graph_client_secret: formData.graph_client_secret || undefined,
        graph_mailbox_upn: formData.graph_mailbox_upn,
        // The backend readiness gate (IsSMTPConfigured) requires host/port/
        // from even when Graph routing handles delivery; these are never
        // dialed. from_email doubles as the MIME From header.
        smtp_host: formData.host || 'graph.local',
        smtp_port: formData.port || 587,
        smtp_from_email: formData.from_email,
        smtp_from_name: formData.from_name || ''
      };
    }
    return {
      graph_send_provider: 'smtp',
      smtp_host: formData.host,
      smtp_port: formData.port || 587,
      smtp_username: plaintext ? '' : formData.username || '',
      // Plaintext relays reject stored credentials, so signal an explicit
      // clear. Blank passwords otherwise mean "keep the saved secret".
      smtp_password: plaintext ? null : formData.password || undefined,
      smtp_from_email: formData.from_email,
      smtp_from_name: formData.from_name || '',
      smtp_encryption: formData.encryption || 'tls',
      smtp_skip_tls_verify: plaintext ? false : formData.skip_tls_verify || false,
      smtp_auth_method: plaintext ? 'none' : formData.auth_method || 'auto'
    };
  }

  export function clearSecret() {
    formData.password = '';
  }
</script>

<div class="pt-6 border-t" style="border-color: var(--ds-border);">
  <h4 class="text-sm font-semibold mb-4" style="color: var(--ds-text);">{t('channel.smtpConfiguration')}</h4>

  <div class="space-y-6">
    <!-- Send Provider -->
    <div class="space-y-4">
      <div>
        <SelectField
          label={t('channel.sendProvider')}
          id="smtp-send-provider"
          labelColor="default"
          ariaLabel={t('channel.sendProvider')}
          options={[
            { value: 'smtp', label: t('channel.sendProviderSmtp') },
            { value: 'graph', label: t('channel.sendProviderGraph') }
          ]}
          bind:value={formData.send_provider}
        />
        <DescriptionText>{t('channel.sendProviderDescription')}</DescriptionText>
      </div>
    </div>

    {#if formData.send_provider === 'graph'}
      <!-- Microsoft Graph client credentials -->
      <div class="space-y-4">
        <div>
          <TextField
            label={t('channel.graphTenantId')}
            required
            labelColor="default"
            type="text"
            placeholder="00000000-0000-0000-0000-000000000000"
            dataTestid="graph-tenant-id"
            bind:value={formData.graph_tenant_id}
          />
          <DescriptionText>{t('channel.graphTenantIdDescription')}</DescriptionText>
        </div>
        <div>
          <TextField
            label={t('channel.graphClientId')}
            required
            labelColor="default"
            type="text"
            placeholder="00000000-0000-0000-0000-000000000000"
            dataTestid="graph-client-id"
            bind:value={formData.graph_client_id}
          />
        </div>
        <div>
          <TextField
            label={t('channel.graphClientSecret')}
            labelColor="default"
            type="password"
            placeholder={t('channel.secretPlaceholder')}
            dataTestid="graph-client-secret"
            bind:value={formData.graph_client_secret}
          />
          <DescriptionText>
            {t('channel.leaveBlankPassword')}
          </DescriptionText>
        </div>
        <div>
          <TextField
            label={t('channel.graphMailboxUpn')}
            required
            labelColor="default"
            type="email"
            placeholder="service@example.com"
            dataTestid="graph-mailbox-upn"
            bind:value={formData.graph_mailbox_upn}
          />
          <DescriptionText>{t('channel.graphMailboxUpnDescription')}</DescriptionText>
        </div>
      </div>
    {:else}
      <!-- Server Settings -->
      <div class="space-y-4">
        <div class="grid grid-cols-2 gap-4">
        <div>
          <TextField
            label={t('channel.smtpHost')}
            required
            labelColor="default"
            type="text"
            placeholder="smtp.example.com"
            bind:value={formData.host}
          />
        </div>
        <div>
          <TextField
            label={t('channel.smtpPort')}
            required
            labelColor="default"
            type="number"
            placeholder="587"
            dataTestid="smtp-port"
            bind:value={formData.port}
          />
        </div>
      </div>

      <div>
        <SelectField
          label={t('channel.smtpEncryption')}
          id="smtp-encryption"
          labelColor="default"
          ariaLabel={t('channel.smtpEncryption')}
          options={[
          { value: 'tls', label: 'STARTTLS (Port 587)' },
          { value: 'ssl', label: 'Implicit TLS (Port 465)' },
          { value: 'none', label: t('channel.noEncryption') }
          ]}
          bind:value={formData.encryption}
        />
      </div>

      {#if formData.encryption === 'none'}
        <div data-testid="smtp-plaintext-warning">
          <AlertBox variant="warning" message={t('channel.smtpNoEncryptionWarning')} />
        </div>
      {:else}
        <div class="flex items-start justify-between gap-4">
          <div>
            <div class="text-sm font-medium" style="color: var(--ds-text);">
              {t('channel.smtpSkipTlsVerify')}
            </div>
            <DescriptionText>{t('channel.smtpSkipTlsVerifyDescription')}</DescriptionText>
          </div>
          <Toggle bind:checked={formData.skip_tls_verify} dataTestid="smtp-skip-tls-verify" />
        </div>
      {/if}
    </div>

    <!-- Authentication -->
    {#if formData.encryption !== 'none'}
      <div class="pt-4 border-t" style="border-color: var(--ds-border);">
        <h5 class="text-sm font-semibold mb-3" style="color: var(--ds-text);">{t('channel.authentication')}</h5>
        <div class="space-y-4">
          <div>
            <SelectField
              label={t('channel.smtpAuthMethod')}
              id="smtp-auth-method"
              labelColor="default"
              ariaLabel={t('channel.smtpAuthMethod')}
              options={[
              { value: 'auto', label: t('channel.smtpAuthAuto') },
              { value: 'plain', label: 'PLAIN' },
              { value: 'login', label: 'LOGIN' },
              { value: 'cram-md5', label: 'CRAM-MD5' },
              { value: 'none', label: t('channel.smtpAuthNone') }
              ]}
              bind:value={formData.auth_method}
            />
            <DescriptionText>{t('channel.smtpAuthMethodDescription')}</DescriptionText>
          </div>
          {#if formData.auth_method !== 'none'}
          <div>
            <TextField
              label={t('channel.smtpUsername')}
              labelColor="default"
              type="text"
              placeholder={t('channel.smtpUsernamePlaceholder')}
              dataTestid="smtp-username"
              bind:value={formData.username}
            />
          </div>
          <div>
            <TextField
              label={t('channel.smtpPassword')}
              labelColor="default"
              type="password"
              placeholder={t('channel.secretPlaceholder')}
              dataTestid="smtp-password"
              bind:value={formData.password}
            />
            <DescriptionText>
              {t('channel.leaveBlankPassword')}
            </DescriptionText>
          </div>
          {/if}
        </div>
      </div>
    {/if}
    {/if}

    <!-- Sender Settings -->
    <div class="pt-4 border-t" style="border-color: var(--ds-border);">
      <h5 class="text-sm font-semibold mb-3" style="color: var(--ds-text);">{t('channel.senderSettings')}</h5>
      <div class="space-y-4">
        <div>
          <TextField
            label={t('channel.smtpFromEmail')}
            required
            labelColor="default"
            type="email"
            placeholder="noreply@example.com"
            bind:value={formData.from_email}
          />
        </div>
        <div>
          <TextField
            label={t('channel.smtpFromName')}
            labelColor="default"
            type="text"
            placeholder={t('channel.smtpFromNamePlaceholder')}
            bind:value={formData.from_name}
          />
        </div>
      </div>
    </div>

    <div class="flex items-center justify-between pt-4 border-t" style="border-color: var(--ds-border);">
      <div>
        <div class="text-sm font-medium" style="color: var(--ds-text);">
          {t('channel.enableSmtp', 'Enable SMTP Channel')}
        </div>
        <div class="text-xs mt-1" style="color: var(--ds-text-subtle);">
          {formData.enabled
            ? t('channel.smtpIsActive', 'SMTP channel is active')
            : t('channel.smtpIsInactive', 'SMTP channel is currently disabled')}
        </div>
      </div>
      <Toggle bind:checked={formData.enabled} />
    </div>
  </div>

  {#if $isSystemAdmin}
    <!-- Test SMTP Section -->
    <div class="mt-6 pt-6 border-t" style="border-color: var(--ds-border);">
      <h5 class="text-sm font-semibold mb-4" style="color: var(--ds-text);">{t('channel.testSmtp')}</h5>
      <div class="space-y-4">
        <div>
          <TextField
            label={t('channel.testEmailAddress')}
            labelColor="default"
            type="email"
            placeholder={t('channel.testEmailPlaceholder')}
            bind:value={testEmail}
          />
        </div>
        <Button
          onclick={testSmtpSettings}
          variant="secondary"
          disabled={loading || (formData.send_provider !== 'graph' && (!formData.host || !formData.from_email)) || (formData.send_provider === 'graph' && !formData.from_email)}
        >
          {t('channel.sendTestEmail')}
        </Button>
      </div>

      {#if testResult}
        {#if testResult.loading}
          <div class="mt-4 flex items-center gap-2 text-sm" style="color: var(--ds-text-subtle);">
            <Spinner size="sm" />
            <span>{testResult.message}</span>
          </div>
        {:else}
          <AlertBox
            variant={testResult.success ? 'success' : 'error'}
            message={testResult.message}
            class="mt-4"
          />
        {/if}
      {/if}
    </div>
  {/if}
</div>
