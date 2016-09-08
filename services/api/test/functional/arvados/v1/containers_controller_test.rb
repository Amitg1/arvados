require 'test_helper'

class Arvados::V1::ContainersControllerTest < ActionController::TestCase
  test 'create' do
    authorize_with :system_user
    post :create, {
      container: {
        command: ['echo', 'hello'],
        container_image: 'test',
        output_path: 'test',
      },
    }
    assert_response :success
  end

  [Container::Queued, Container::Complete].each do |state|
    test "cannot get auth in #{state} state" do
      authorize_with :dispatch1
      get :auth, id: containers(:queued).uuid
      assert_response 403
    end
  end

  test 'cannot get auth with wrong token' do
    authorize_with :dispatch1
    c = containers(:queued)
    assert c.lock, show_errors(c)

    authorize_with :system_user
    get :auth, id: c.uuid
    assert_response 403
  end

  test 'get auth' do
    authorize_with :dispatch1
    c = containers(:queued)
    assert c.lock, show_errors(c)
    get :auth, id: c.uuid
    assert_response :success
    assert_operator 32, :<, json_response['api_token'].length
    assert_equal 'arvados#apiClientAuthorization', json_response['kind']
  end

  test 'no auth in container response' do
    authorize_with :dispatch1
    c = containers(:queued)
    assert c.lock, show_errors(c)
    get :show, id: c.uuid
    assert_response :success
    assert_nil json_response['auth']
  end

  test "lock container" do
    authorize_with :dispatch1
    post :lock, {id: containers(:queued).uuid}
    assert_response :success
    container = Container.where(uuid: containers(:queued).uuid).first
    assert_equal 'Locked', container.state
    assert_not_nil container.locked_by_uuid
    assert_not_nil container.auth_uuid
  end

  test "unlock container" do
    authorize_with :dispatch1
    post :unlock, {id: containers(:locked).uuid}
    assert_response :success
    container = Container.where(uuid: container.uuid).first
    assert_equal 'Queued', container.state
    assert_nil container.locked_by_uuid
    assert_nil container.auth_uuid
  end

  [
    [:queued, :lock, :success, 'Locked'],
    [:queued, :unlock, 422, 'Queued'],
    [:locked, :lock, 422, 'Locked'],
    [:running, :lock, 422, 'Running'],
    [:running, :unlock, 422, 'Running'],
  ].each do |fixture, action, response, state|
    test "state transitions from #{fixture } to #{action}" do
      authorize_with :dispatch1
      uuid = containers(fixture).uuid
      post action, {id: uuid}
      assert_response response
      assert_equal state, Container.where(uuid: uuid).first.state
    end
  end
end
