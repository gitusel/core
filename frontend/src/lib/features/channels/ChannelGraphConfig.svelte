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

  let {
    channelId,
    formData = $bindable({
      tenant_id: '',
      client_id: '',
      client_secret: '',
      secret_stored: false,
      from_email: '',
      from_name: '',
      enabled: false
    }),
    onSave = () => {}
  } = $props();

  let testResult = $state(null);
  let testEmail = $state('');
  let loading = $state(false);

  function bareEmail(value) {
    return /^[^\s@]+@[^\s@]+$/.test(value?.trim() ?? '');
  }

  async function testGraphSettings() {
    if (!$isSystemAdmin || loading) return;
    if (!channelId || !formData.tenant_id?.trim() || !formData.client_id?.trim() || !bareEmail(formData.from_email)) {
      testResult = { success: false, message: t('channel.graphFieldsRequired') };
      return;
    }
    if (!formData.client_secret?.trim() && !formData.secret_stored) {
      testResult = { success: false, message: t('channel.clientSecretRequired') };
      return;
    }
    if (!testEmail) {
      testResult = { success: false, message: t('channel.testEmailRequired') };
      return;
    }

    testResult = { success: true, message: t('channel.sendingTestEmail'), loading: true };
    loading = true;
    try {
      await channels.updateConfig(channelId, getConfig());
      clearSecret();
      const result = await channels.testWithEmail(channelId, testEmail);
      if (result.success) {
        testResult = { success: true, message: t('channel.testEmailSent'), loading: false };
        onSave();
      } else {
        testResult = {
          success: false,
          message: `${t('channel.testEmailFailed')}: ${result.message || 'Unknown error'}`,
          loading: false
        };
      }
    } catch (error) {
      console.error('Failed to test Graph channel:', error);
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
    if (!formData.tenant_id?.trim()) {
      return { valid: false, message: t('channel.graphTenantRequired') };
    }
    if (!formData.client_id?.trim()) {
      return { valid: false, message: t('channel.clientIdRequired') };
    }
    if (!bareEmail(formData.from_email)) {
      return { valid: false, message: t('channel.graphFromEmailRequired') };
    }
    if (formData.enabled && !formData.client_secret?.trim() && !formData.secret_stored) {
      return { valid: false, message: t('channel.clientSecretRequired') };
    }
    return { valid: true };
  }

  export function getConfig() {
    return {
      graph_tenant_id: formData.tenant_id.trim(),
      graph_client_id: formData.client_id.trim(),
      // Blank means keep the stored ciphertext. JSON null is the explicit clear.
      graph_client_secret: formData.client_secret || undefined,
      graph_from_email: formData.from_email.trim(),
      graph_from_name: formData.from_name?.trim() || ''
    };
  }

  export function clearSecret() {
    if (formData.client_secret) {
      formData.secret_stored = true;
    }
    formData.client_secret = '';
  }
</script>

<div class="pt-6 border-t" style="border-color: var(--ds-border);">
  <h4 class="text-sm font-semibold mb-4" style="color: var(--ds-text);">{t('channel.graphConfiguration')}</h4>
  <DescriptionText>{t('channel.graphHelp')}</DescriptionText>

  <div class="space-y-6 mt-4">
    <div class="space-y-4">
      <TextField
        label={t('channel.tenantId')}
        required
        labelColor="default"
        placeholder="directory-tenant-id"
        dataTestid="graph-tenant-id"
        bind:value={formData.tenant_id}
      />
      <TextField
        label={t('channel.clientId')}
        required
        labelColor="default"
        placeholder="application-client-id"
        dataTestid="graph-client-id"
        bind:value={formData.client_id}
      />
      <div>
        <TextField
          label={t('channel.clientSecret')}
          labelColor="default"
          type="password"
          placeholder={formData.secret_stored ? t('channel.leaveBlankToKeep') : t('channel.secretPlaceholder')}
          dataTestid="graph-client-secret"
          bind:value={formData.client_secret}
        />
        {#if formData.secret_stored && !formData.client_secret}
          <DescriptionText>{t('channel.secretSaved')}</DescriptionText>
        {/if}
      </div>
    </div>

    <div class="pt-4 border-t space-y-4" style="border-color: var(--ds-border);">
      <h5 class="text-sm font-semibold" style="color: var(--ds-text);">{t('channel.senderSettings')}</h5>
      <TextField
        label={t('channel.graphFromEmail')}
        required
        labelColor="default"
        type="email"
        placeholder="noreply@contoso.example"
        dataTestid="graph-from-email"
        bind:value={formData.from_email}
      />
      <TextField
        label={t('channel.graphFromName')}
        labelColor="default"
        placeholder={t('channel.graphFromNamePlaceholder')}
        dataTestid="graph-from-name"
        bind:value={formData.from_name}
      />
    </div>

    <div class="flex items-center justify-between pt-4 border-t" style="border-color: var(--ds-border);">
      <div>
        <div class="text-sm font-medium" style="color: var(--ds-text);">{t('channel.enableGraph')}</div>
        <div class="text-xs mt-1" style="color: var(--ds-text-subtle);">
          {formData.enabled ? t('channel.graphIsActive') : t('channel.graphIsInactive')}
        </div>
      </div>
      <Toggle bind:checked={formData.enabled} dataTestid="graph-enabled" />
    </div>
  </div>

  {#if $isSystemAdmin}
    <div class="mt-6 pt-6 border-t" style="border-color: var(--ds-border);">
      <h5 class="text-sm font-semibold mb-4" style="color: var(--ds-text);">{t('channel.testGraph')}</h5>
      <div class="space-y-4">
        <TextField
          label={t('channel.testEmailAddress')}
          labelColor="default"
          type="email"
          placeholder={t('channel.testEmailPlaceholder')}
          dataTestid="graph-test-email"
          bind:value={testEmail}
        />
        <Button onclick={testGraphSettings} variant="secondary" disabled={loading} dataTestid="graph-send-test">
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
